// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux
// +build linux

package probe

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/DataDog/datadog-go/statsd"
	manager "github.com/DataDog/ebpf-manager"
	lib "github.com/cilium/ebpf"
	"github.com/pkg/errors"

	"github.com/DataDog/datadog-agent/pkg/ebpf/bytecode"
	pconfig "github.com/DataDog/datadog-agent/pkg/process/config"
	"github.com/DataDog/datadog-agent/pkg/security/config"
	"github.com/DataDog/datadog-agent/pkg/security/ebpf"
	"github.com/DataDog/datadog-agent/pkg/security/ebpf/kernel"
	"github.com/DataDog/datadog-agent/pkg/security/ebpf/probes"
	seclog "github.com/DataDog/datadog-agent/pkg/security/log"
	"github.com/DataDog/datadog-agent/pkg/security/probe/constantfetch"
	"github.com/DataDog/datadog-agent/pkg/security/secl/compiler/eval"
	"github.com/DataDog/datadog-agent/pkg/security/secl/model"
	"github.com/DataDog/datadog-agent/pkg/security/secl/rules"
	"github.com/DataDog/datadog-agent/pkg/security/utils"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

// EventHandler represents an handler for the events sent by the probe
type EventHandler interface {
	HandleEvent(event *Event)
	HandleCustomEvent(rule *rules.Rule, event *CustomEvent)
}

// Probe represents the runtime security eBPF probe in charge of
// setting up the required kProbes and decoding events sent from the kernel
type Probe struct {
	// Constants and configuration
	manager        *manager.Manager
	managerOptions manager.Options
	config         *config.Config
	statsdClient   *statsd.Client
	startTime      time.Time
	kernelVersion  *kernel.Version
	_              uint32 // padding for goarch=386
	ctx            context.Context
	cancelFnc      context.CancelFunc
	wg             sync.WaitGroup
	// Events section
	handler   EventHandler
	monitor   *Monitor
	resolvers *Resolvers
	event     *Event
	perfMap   *manager.PerfMap
	reOrderer *ReOrderer
	scrubber  *pconfig.DataScrubber

	// Approvers / discarders section
	erpc               *ERPC
	pidDiscarders      *pidDiscarders
	inodeDiscarders    *inodeDiscarders
	flushingDiscarders int64
	approvers          map[eval.EventType]activeApprovers

	constantOffsets map[string]uint64
}

// GetResolvers returns the resolvers of Probe
func (p *Probe) GetResolvers() *Resolvers {
	return p.resolvers
}

// Map returns a map by its name
func (p *Probe) Map(name string) (*lib.Map, error) {
	if p.manager == nil {
		return nil, fmt.Errorf("failed to get map '%s', manager is null", name)
	}
	m, ok, err := p.manager.GetMap(name)
	if err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("failed to get map '%s'", name)
	}
	return m, nil
}

func (p *Probe) detectKernelVersion() error {
	kernelVersion, err := kernel.NewKernelVersion()
	if err != nil {
		return errors.Wrap(err, "unable to detect the kernel version")
	}
	p.kernelVersion = kernelVersion
	return nil
}

// GetKernelVersion computes and returns the running kernel version
func (p *Probe) GetKernelVersion() (*kernel.Version, error) {
	if err := p.detectKernelVersion(); err != nil {
		return nil, err
	}
	return p.kernelVersion, nil
}

// VerifyOSVersion returns an error if the current kernel version is not supported
func (p *Probe) VerifyOSVersion() error {
	if !p.kernelVersion.IsRH7Kernel() && !p.kernelVersion.IsRH8Kernel() && p.kernelVersion.Code < kernel.Kernel4_15 {
		return errors.Errorf("the following kernel is not supported: %s", p.kernelVersion)
	}
	return nil
}

// Init initializes the probe
func (p *Probe) Init(client *statsd.Client) error {
	p.startTime = time.Now()

	var err error
	var bytecodeReader bytecode.AssetReader

	useSyscallWrapper := false
	openSyscall, err := manager.GetSyscallFnName("open")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(openSyscall, "SyS_") && !strings.HasPrefix(openSyscall, "sys_") {
		useSyscallWrapper = true
	}

	if p.config.EnableRuntimeCompiler {
		bytecodeReader, err = getRuntimeCompiledProbe(p.config, useSyscallWrapper)
		if err != nil {
			log.Warnf("error compiling runtime-security probe, falling back to pre-compiled: %s", err)
		} else {
			defer bytecodeReader.Close()
		}
	}

	// fallback to pre-compiled version
	if bytecodeReader == nil {
		asset := "runtime-security"
		if useSyscallWrapper {
			asset += "-syscall-wrapper"
		}

		bytecodeReader, err = bytecode.GetReader(p.config.BPFDir, asset+".o")
		if err != nil {
			return err
		}
		defer bytecodeReader.Close()
	}

	var ok bool
	if p.perfMap, ok = p.manager.GetPerfMap("events"); !ok {
		return errors.New("couldn't find events perf map")
	}

	p.perfMap.PerfMapOptions = manager.PerfMapOptions{
		DataHandler: p.reOrderer.HandleEvent,
		LostHandler: p.handleLostEvents,
	}

	if os.Getenv("RUNTIME_SECURITY_TESTSUITE") != "true" {
		p.managerOptions.ConstantEditors = append(p.managerOptions.ConstantEditors, manager.ConstantEditor{
			Name:  "runtime_discarded",
			Value: uint64(1),
		})
	}

	if selectors, exists := probes.SelectorsPerEventType["*"]; exists {
		p.managerOptions.ActivatedProbes = append(p.managerOptions.ActivatedProbes, selectors...)
	}

	if err := p.manager.InitWithOptions(bytecodeReader, p.managerOptions); err != nil {
		return errors.Wrap(err, "failed to init manager")
	}

	pidDiscardersMap, err := p.Map("pid_discarders")
	if err != nil {
		return err
	}
	p.pidDiscarders = newPidDiscarders(pidDiscardersMap, p.erpc)

	inodeDiscardersMap, err := p.Map("inode_discarders")
	if err != nil {
		return err
	}

	discarderRevisionsMap, err := p.Map("discarder_revisions")
	if err != nil {
		return err
	}

	if p.inodeDiscarders, err = newInodeDiscarders(inodeDiscardersMap, discarderRevisionsMap, p.erpc, p.resolvers.DentryResolver); err != nil {
		return err
	}

	if err := p.resolvers.Start(p.ctx); err != nil {
		return err
	}

	p.monitor, err = NewMonitor(p, client)
	if err != nil {
		return err
	}

	return nil
}

// Start the runtime security probe
func (p *Probe) Start() error {
	p.wg.Add(1)
	go p.reOrderer.Start(&p.wg)

	if err := p.manager.Start(); err != nil {
		return err
	}

	return p.monitor.Start(p.ctx, &p.wg)
}

// SetEventHandler set the probe event handler
func (p *Probe) SetEventHandler(handler EventHandler) {
	p.handler = handler
}

// DispatchEvent sends an event to the probe event handler
func (p *Probe) DispatchEvent(event *Event, size uint64, CPU int, perfMap *manager.PerfMap) {
	seclog.TraceTagf(event.GetEventType(), "Dispatching event %s", event)

	if p.handler != nil {
		p.handler.HandleEvent(event)
	}

	// Process after evaluation because some monitors need the DentryResolver to have been called first.
	p.monitor.ProcessEvent(event, size, CPU, perfMap)
}

// DispatchCustomEvent sends a custom event to the probe event handler
func (p *Probe) DispatchCustomEvent(rule *rules.Rule, event *CustomEvent) {
	seclog.TraceTagf(event.GetEventType(), "Dispatching custom event %s", event)

	if p.handler != nil && p.config.AgentMonitoringEvents {
		p.handler.HandleCustomEvent(rule, event)
	}
}

// SendStats sends statistics about the probe to Datadog
func (p *Probe) SendStats() error {
	return p.monitor.SendStats()
}

// GetMonitor returns the monitor of the probe
func (p *Probe) GetMonitor() *Monitor {
	return p.monitor
}

func (p *Probe) handleLostEvents(CPU int, count uint64, perfMap *manager.PerfMap, manager *manager.Manager) {
	seclog.Tracef("lost %d events", count)
	p.monitor.perfBufferMonitor.CountLostEvent(count, perfMap, CPU)
}

func (p *Probe) zeroEvent() *Event {
	*p.event = eventZero
	return p.event
}

func (p *Probe) unmarshalContexts(data []byte, event *Event) (int, error) {
	read, err := model.UnmarshalBinary(data, &event.ProcessContext, &event.SpanContext, &event.ContainerContext)
	if err != nil {
		return 0, err
	}

	return read, nil
}

func (p *Probe) invalidateDentry(mountID uint32, inode uint64) {
	// sanity check
	if mountID == 0 || inode == 0 {
		seclog.Tracef("invalid mount_id/inode tuple %d:%d", mountID, inode)
		return
	}

	seclog.Tracef("remove dentry cache entry for inode %d", inode)
	p.resolvers.DentryResolver.DelCacheEntry(mountID, inode)
}

func (p *Probe) handleEvent(CPU uint64, data []byte) {
	offset := 0
	event := p.zeroEvent()
	dataLen := uint64(len(data))

	read, err := event.UnmarshalBinary(data)
	if err != nil {
		log.Errorf("failed to decode event: %s", err)
		return
	}
	offset += read

	eventType := event.GetEventType()
	p.monitor.perfBufferMonitor.CountEvent(eventType, event.TimestampRaw, 1, dataLen, p.perfMap, int(CPU))

	// no need to dispatch events
	switch eventType {
	case model.MountReleasedEventType:
		if _, err = event.MountReleased.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode mount released event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}

		// Remove all dentry entries belonging to the mountID
		p.resolvers.DentryResolver.DelCacheEntries(event.MountReleased.MountID)

		if p.resolvers.MountResolver.IsOverlayFS(event.MountReleased.MountID) {
			p.inodeDiscarders.setRevision(event.MountReleased.MountID, event.MountReleased.DiscarderRevision)
		}

		// Delete new mount point from cache
		if err = p.resolvers.MountResolver.Delete(event.MountReleased.MountID); err != nil {
			log.Warnf("failed to delete mount point %d from cache: %s", event.MountReleased.MountID, err)
		}
		return
	case model.InvalidateDentryEventType:
		if _, err = event.InvalidateDentry.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode invalidate dentry event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}

		p.invalidateDentry(event.InvalidateDentry.MountID, event.InvalidateDentry.Inode)

		return
	case model.ArgsEnvsEventType:
		if _, err = event.ArgsEnvs.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode args envs event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}

		p.resolvers.ProcessResolver.UpdateArgsEnvs(&event.ArgsEnvs)

		return
	}

	read, err = p.unmarshalContexts(data[offset:], event)
	if err != nil {
		log.Errorf("failed to decode event `%s`: %s", eventType, err)
		return
	}
	offset += read

	switch eventType {
	case model.FileMountEventType:
		if _, err = event.Mount.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode mount event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}

		// Resolve mount point
		event.SetMountPoint(&event.Mount)
		// Resolve root
		event.SetMountRoot(&event.Mount)
		// Insert new mount point in cache
		err = p.resolvers.MountResolver.Insert(event.Mount)
		if err != nil {
			log.Errorf("failed to insert mount event: %v", err)
		}

		// There could be entries of a previous mount_id in the cache for instance,
		// runc does the following : it bind mounts itself (using /proc/exe/self),
		// opens a file descriptor on the new file with O_CLOEXEC then umount the bind mount using
		// MNT_DETACH. It then does an exec syscall, that will cause the fd to be closed.
		// Our dentry resolution of the exec event causes the inode/mount_id to be put in cache,
		// so we remove all dentry entries belonging to the mountID.
		p.resolvers.DentryResolver.DelCacheEntries(event.Mount.MountID)
	case model.FileUmountEventType:
		if _, err = event.Umount.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode umount event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}
	case model.FileOpenEventType:
		if _, err = event.Open.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode open event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}
	case model.FileMkdirEventType:
		if _, err = event.Mkdir.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode mkdir event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}
	case model.FileRmdirEventType:
		if _, err = event.Rmdir.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode rmdir event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}

		if event.Rmdir.Retval >= 0 {
			// defer it do ensure that it will be done after the dispatch that could re-add it
			defer p.invalidateDentry(event.Rmdir.File.MountID, event.Rmdir.File.Inode)
		}
	case model.FileUnlinkEventType:
		if _, err = event.Unlink.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode unlink event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}

		if event.Unlink.Retval >= 0 {
			// defer it do ensure that it will be done after the dispatch that could re-add it
			defer p.invalidateDentry(event.Unlink.File.MountID, event.Unlink.File.Inode)
		}
	case model.FileRenameEventType:
		if _, err = event.Rename.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode rename event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}

		if event.Rename.Retval >= 0 {
			// defer it do ensure that it will be done after the dispatch that could re-add it
			defer p.invalidateDentry(event.Rename.New.MountID, event.Rename.New.Inode)
		}
	case model.FileChmodEventType:
		if _, err = event.Chmod.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode chmod event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}
	case model.FileChownEventType:
		if _, err = event.Chown.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode chown event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}
	case model.FileUtimesEventType:
		if _, err = event.Utimes.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode utime event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}
	case model.FileLinkEventType:
		if _, err = event.Link.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode link event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}

		// need to invalidate as now nlink > 1
		if event.Link.Retval >= 0 {
			// defer it do ensure that it will be done after the dispatch that could re-add it
			defer p.invalidateDentry(event.Link.Source.MountID, event.Link.Source.Inode)
		}
	case model.FileSetXAttrEventType:
		if _, err = event.SetXAttr.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode setxattr event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}
	case model.FileRemoveXAttrEventType:
		if _, err = event.RemoveXAttr.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode removexattr event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}
	case model.ForkEventType:
		if _, err = event.UnmarshalProcess(data[offset:]); err != nil {
			log.Errorf("failed to decode fork event: %s (offset %d, len %d)", err, offset, dataLen)
			return
		}

		if IsKThread(event.processCacheEntry.PPid, event.processCacheEntry.Pid) {
			return
		}

		p.resolvers.ProcessResolver.ApplyBootTime(event.processCacheEntry)

		p.resolvers.ProcessResolver.AddForkEntry(event.ProcessContext.Pid, event.processCacheEntry)
	case model.ExecEventType:
		// unmarshal and fill event.processCacheEntry
		if _, err = event.UnmarshalProcess(data[offset:]); err != nil {
			log.Errorf("failed to decode exec event: %s (offset %d, len %d)", err, offset, len(data))
			return
		}

		if err = p.resolvers.ProcessResolver.ResolveNewProcessCacheEntryContext(event.processCacheEntry); err != nil {
			log.Debugf("failed to resolve new process cache entry context: %s", err)
		}

		p.resolvers.ProcessResolver.AddExecEntry(event.ProcessContext.Pid, event.processCacheEntry)

		// copy some of the field from the entry
		event.Exec.Process = event.processCacheEntry.Process
		event.Exec.FileFields = event.processCacheEntry.Process.FileFields
	case model.ExitEventType:
		// do nothing
	case model.SetuidEventType:
		if _, err = event.SetUID.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode setuid event: %s (offset %d, len %d)", err, offset, len(data))
			return
		}
		defer p.resolvers.ProcessResolver.UpdateUID(event.ProcessContext.Pid, event)
	case model.SetgidEventType:
		if _, err = event.SetGID.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode setgid event: %s (offset %d, len %d)", err, offset, len(data))
			return
		}
		defer p.resolvers.ProcessResolver.UpdateGID(event.ProcessContext.Pid, event)
	case model.CapsetEventType:
		if _, err = event.Capset.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode capset event: %s (offset %d, len %d)", err, offset, len(data))
			return
		}
		defer p.resolvers.ProcessResolver.UpdateCapset(event.ProcessContext.Pid, event)
	case model.SELinuxEventType:
		if _, err = event.SELinux.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode selinux event: %s (offset %d, len %d)", err, offset, len(data))
			return
		}
	case model.BPFEventType:
		if _, err = event.BPF.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode bpf event: %s (offset %d, len %d)", err, offset, len(data))
			return
		}
	case model.PTraceEventType:
		if _, err = event.PTrace.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode ptrace event: %s (offset %d, len %d)", err, offset, len(data))
			return
		}
		// resolve tracee process context
		cacheEntry := event.resolvers.ProcessResolver.Resolve(event.PTrace.PID, event.PTrace.PID)
		if cacheEntry != nil {
			event.PTrace.Tracee = cacheEntry.ProcessContext
		}
	case model.MMapEventType:
		if _, err = event.MMap.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode mmap event: %s (offset %d, len %d)", err, offset, len(data))
			return
		}
	case model.MProtectEventType:
		if _, err = event.MProtect.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode mprotect event: %s (offset %d, len %d)", err, offset, len(data))
			return
		}
	case model.LoadModuleEventType:
		if _, err = event.LoadModule.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode load_module event: %s (offset %d, len %d)", err, offset, len(data))
			return
		}
	case model.UnloadModuleEventType:
		if _, err = event.UnloadModule.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode unload_module event: %s (offset %d, len %d)", err, offset, len(data))
		}
	case model.SignalEventType:
		if _, err = event.Signal.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode signal event: %s (offset %d, len %d)", err, offset, len(data))
			return
		}
		// resolve target process context
		cacheEntry := event.resolvers.ProcessResolver.Resolve(event.Signal.PID, event.Signal.PID)
		if cacheEntry != nil {
			event.Signal.Target = cacheEntry.ProcessContext
		}
	case model.SpliceEventType:
		if _, err = event.Splice.UnmarshalBinary(data[offset:]); err != nil {
			log.Errorf("failed to decode splice event: %s (offset %d, len %d)", err, offset, len(data))
			return
		}
	default:
		log.Errorf("unsupported event type %d", eventType)
		return
	}

	// resolve event context
	if eventType != model.ExitEventType {
		event.ResolveProcessCacheEntry()
		event.ProcessContext = event.processCacheEntry.ProcessContext
	} else {
		if IsKThread(event.ProcessContext.PPid, event.ProcessContext.Pid) {
			return
		}

		defer p.resolvers.ProcessResolver.DeleteEntry(event.ProcessContext.Pid, event.ResolveEventTimestamp())
	}

	p.DispatchEvent(event, dataLen, int(CPU), p.perfMap)

	// flush exited process
	p.resolvers.ProcessResolver.DequeueExited()
}

// OnRuleMatch is called when a rule matches just before sending
func (p *Probe) OnRuleMatch(rule *rules.Rule, event *Event) {
	// ensure that all the fields are resolved before sending
	event.ResolveContainerID(&event.ContainerContext)
	event.ResolveContainerTags(&event.ContainerContext)
}

// OnNewDiscarder is called when a new discarder is found
func (p *Probe) OnNewDiscarder(rs *rules.RuleSet, event *Event, field eval.Field, eventType eval.EventType) error {
	// discarders disabled
	if !p.config.EnableDiscarders {
		return nil
	}

	if atomic.LoadInt64(&p.flushingDiscarders) == 1 {
		return nil
	}

	seclog.Tracef("New discarder of type %s for field %s", eventType, field)

	if handler, ok := allDiscarderHandlers[eventType]; ok {
		return handler(rs, event, p, Discarder{Field: field})
	}

	return nil
}

// ApplyFilterPolicy is called when a passing policy for an event type is applied
func (p *Probe) ApplyFilterPolicy(eventType eval.EventType, mode PolicyMode, flags PolicyFlag) error {
	log.Infof("Setting in-kernel filter policy to `%s` for `%s`", mode, eventType)
	table, err := p.Map("filter_policy")
	if err != nil {
		return errors.Wrap(err, "unable to find policy table")
	}

	et := model.ParseEvalEventType(eventType)
	if et == model.UnknownEventType {
		return errors.New("unable to parse the eval event type")
	}

	policy := &FilterPolicy{
		Mode:  mode,
		Flags: flags,
	}

	return table.Put(ebpf.Uint32MapItem(et), policy)
}

// SetApprovers applies approvers and removes the unused ones
func (p *Probe) SetApprovers(eventType eval.EventType, approvers rules.Approvers) error {
	handler, exists := allApproversHandlers[eventType]
	if !exists {
		return nil
	}

	newApprovers, err := handler(p, approvers)
	if err != nil {
		log.Errorf("Error while adding approvers fallback in-kernel policy to `%s` for `%s`: %s", PolicyModeAccept, eventType, err)
	}

	for _, newApprover := range newApprovers {
		seclog.Tracef("Applying approver %+v", newApprover)
		if err := newApprover.Apply(p); err != nil {
			return err
		}
	}

	if previousApprovers, exist := p.approvers[eventType]; exist {
		previousApprovers.Sub(newApprovers)
		for _, previousApprover := range previousApprovers {
			seclog.Tracef("Removing previous approver %+v", previousApprover)
			if err := previousApprover.Remove(p); err != nil {
				return err
			}
		}
	}

	p.approvers[eventType] = newApprovers
	return nil
}

// SelectProbes applies the loaded set of rules and returns a report
// of the applied approvers for it.
func (p *Probe) SelectProbes(rs *rules.RuleSet) error {
	var activatedProbes []manager.ProbesSelector

	for eventType, selectors := range probes.SelectorsPerEventType {
		if eventType == "*" || rs.HasRulesForEventType(eventType) {
			activatedProbes = append(activatedProbes, selectors...)
		}
	}

	// Add syscall monitor probes
	if p.config.SyscallMonitor {
		activatedProbes = append(activatedProbes, probes.SyscallMonitorSelectors...)
	}

	// Print the list of unique probe identification IDs that are registered
	var selectedIDs []manager.ProbeIdentificationPair
	for _, selector := range activatedProbes {
		for _, id := range selector.GetProbesIdentificationPairList() {
			var exists bool
			for _, selectedID := range selectedIDs {
				if selectedID.Matches(id) {
					exists = true
				}
			}
			if !exists {
				selectedIDs = append(selectedIDs, id)
				seclog.Tracef("probe %s selected", id)
			}
		}
	}

	enabledEventsMap, err := p.Map("enabled_events")
	if err != nil {
		return err
	}

	enabledEvents := uint64(0)
	for _, eventName := range rs.GetEventTypes() {
		if eventName != "*" {
			eventType := model.ParseEvalEventType(eventName)
			if eventType == model.UnknownEventType {
				return fmt.Errorf("unknown event type '%s'", eventName)
			}
			enabledEvents |= 1 << (eventType - 1)
		}
	}

	// We might end up missing events during the snapshot. Ultimately we might want to stop the rules evaluation but
	// not the perf map entirely. For now this will do though :)
	if err := p.perfMap.Pause(); err != nil {
		return err
	}
	defer func() {
		if err := p.perfMap.Resume(); err != nil {
			log.Errorf("failed to resume perf map: %s", err)
		}
	}()

	if err := enabledEventsMap.Put(ebpf.ZeroUint32MapItem, enabledEvents); err != nil {
		return errors.Wrap(err, "failed to set enabled events")
	}

	return p.manager.UpdateActivatedProbes(activatedProbes)
}

// FlushDiscarders removes all the discarders
func (p *Probe) FlushDiscarders() error {
	log.Debug("Freezing discarders")

	flushingMap, err := p.Map("flushing_discarders")
	if err != nil {
		return err
	}

	if err := flushingMap.Put(ebpf.ZeroUint32MapItem, uint32(1)); err != nil {
		return errors.Wrap(err, "failed to set flush_discarders flag")
	}

	unfreezeDiscarders := func() {
		atomic.StoreInt64(&p.flushingDiscarders, 0)

		if err := flushingMap.Put(ebpf.ZeroUint32MapItem, uint32(0)); err != nil {
			log.Errorf("Failed to reset flush_discarders flag: %s", err)
		}

		log.Debugf("Unfreezing discarders")
	}
	defer unfreezeDiscarders()

	// Sleeping a bit to avoid races with executing kprobes and setting discarders
	if !atomic.CompareAndSwapInt64(&p.flushingDiscarders, 0, 1) {
		return errors.New("already flushing discarders")
	}

	var discardedInodes []inodeDiscarder
	var mapValue [256]byte

	var inode inodeDiscarder
	for entries := p.inodeDiscarders.Iterate(); entries.Next(&inode, unsafe.Pointer(&mapValue[0])); {
		discardedInodes = append(discardedInodes, inode)
	}

	var discardedPids []uint32
	for pid, entries := uint32(0), p.pidDiscarders.Iterate(); entries.Next(&pid, unsafe.Pointer(&mapValue[0])); {
		discardedPids = append(discardedPids, pid)
	}

	discarderCount := len(discardedInodes) + len(discardedPids)
	if discarderCount == 0 {
		log.Debugf("No discarder found")
		return nil
	}

	flushWindow := time.Second * time.Duration(p.config.FlushDiscarderWindow)
	delay := flushWindow / time.Duration(discarderCount)

	flushDiscarders := func() {
		log.Debugf("Flushing discarders")

		for _, inode := range discardedInodes {
			if err := p.inodeDiscarders.expireInodeDiscarder(inode.PathKey.MountID, inode.PathKey.Inode); err != nil {
				seclog.Tracef("Failed to flush discarder for inode %d: %s", inode, err)
			}

			discarderCount--
			if discarderCount > 0 {
				time.Sleep(delay)
			}
		}

		for _, pid := range discardedPids {
			if err := p.pidDiscarders.Delete(unsafe.Pointer(&pid)); err != nil {
				seclog.Tracef("Failed to flush discarder for pid %d: %s", pid, err)
			}

			discarderCount--
			if discarderCount > 0 {
				time.Sleep(delay)
			}
		}
	}

	if p.config.FlushDiscarderWindow != 0 {
		go flushDiscarders()
	} else {
		flushDiscarders()
	}

	return nil
}

// Snapshot runs the different snapshot functions of the resolvers that
// require to sync with the current state of the system
func (p *Probe) Snapshot() error {
	return p.resolvers.Snapshot()
}

// Close the probe
func (p *Probe) Close() error {
	// Cancelling the context will stop the reorderer = we won't dequeue events anymore and new events from the
	// perf map reader are ignored
	p.cancelFnc()

	// we wait until both the reorderer and the monitor are stopped
	p.wg.Wait()

	// Stopping the manager will stop the perf map reader and unload eBPF programs
	if err := p.manager.Stop(manager.CleanAll); err != nil {
		return err
	}

	// when we reach this point, we do not generate nor consume events anymore, we can close the resolvers
	return p.resolvers.Close()
}

// GetDebugStats returns the debug stats
func (p *Probe) GetDebugStats() map[string]interface{} {
	debug := map[string]interface{}{
		"start_time": p.startTime.String(),
	}
	// TODO(Will): add manager state
	return debug
}

// NewRuleSet returns a new rule set
func (p *Probe) NewRuleSet(opts *rules.Opts) *rules.RuleSet {
	eventCtor := func() eval.Event {
		return NewEvent(p.resolvers, p.scrubber)
	}
	opts.WithLogger(&seclog.PatternLogger{})

	return rules.NewRuleSet(&Model{probe: p}, eventCtor, opts)
}

// NewProbe instantiates a new runtime security agent probe
func NewProbe(config *config.Config, client *statsd.Client) (*Probe, error) {
	erpc, err := NewERPC()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	p := &Probe{
		config:         config,
		approvers:      make(map[eval.EventType]activeApprovers),
		manager:        ebpf.NewRuntimeSecurityManager(),
		managerOptions: ebpf.NewDefaultOptions(),
		ctx:            ctx,
		cancelFnc:      cancel,
		statsdClient:   client,
		erpc:           erpc,
	}

	if err = p.detectKernelVersion(); err != nil {
		// we need the kernel version to start, fail if we can't get it
		return nil, err
	}
	if err = p.VerifyOSVersion(); err != nil {
		log.Warnf("the current kernel isn't officially supported, some features might not work properly: %v", err)
	}

	p.ensureConfigDefaults()

	numCPU, err := utils.NumCPU()
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse CPU count")
	}
	p.managerOptions.MapSpecEditors = probes.AllMapSpecEditors(numCPU)

	if !p.config.EnableKernelFilters {
		log.Warn("Forcing in-kernel filter policy to `pass`: filtering not enabled")
	}

	if p.config.SyscallMonitor {
		// Add syscall monitor probes
		p.managerOptions.ActivatedProbes = append(p.managerOptions.ActivatedProbes, probes.SyscallMonitorSelectors...)
	}

	p.constantOffsets, err = GetOffsetConstants(config, p)
	if err != nil {
		log.Warnf("constant fetcher failed: %v", err)
		return nil, err
	}

	p.managerOptions.ConstantEditors = append(p.managerOptions.ConstantEditors, constantfetch.CreateConstantEditors(p.constantOffsets)...)

	// Add global constant editors
	p.managerOptions.ConstantEditors = append(p.managerOptions.ConstantEditors,
		manager.ConstantEditor{
			Name:  "runtime_pid",
			Value: uint64(utils.Getpid()),
		},
		manager.ConstantEditor{
			Name:  "do_fork_input",
			Value: getDoForkInput(p),
		},
		manager.ConstantEditor{
			Name:  "mount_id_offset",
			Value: getMountIDOffset(p),
		},
		manager.ConstantEditor{
			Name:  "getattr2",
			Value: getAttr2(p),
		},
		manager.ConstantEditor{
			Name:  "vfs_unlink_dentry_position",
			Value: getVFSLinkDentryPosition(p),
		},
		manager.ConstantEditor{
			Name:  "vfs_mkdir_dentry_position",
			Value: getVFSMKDirDentryPosition(p),
		},
		manager.ConstantEditor{
			Name:  "vfs_link_target_dentry_position",
			Value: getVFSLinkTargetDentryPosition(p),
		},
		manager.ConstantEditor{
			Name:  "vfs_setxattr_dentry_position",
			Value: getVFSSetxattrDentryPosition(p),
		},
		manager.ConstantEditor{
			Name:  "vfs_removexattr_dentry_position",
			Value: getVFSRemovexattrDentryPosition(p),
		},
		manager.ConstantEditor{
			Name:  "vfs_rename_input_type",
			Value: getVFSRenameInputType(p),
		},
		manager.ConstantEditor{
			Name:  "check_helper_call_input",
			Value: getCheckHelperCallInputType(p),
		},
	)
	p.managerOptions.ConstantEditors = append(p.managerOptions.ConstantEditors, DiscarderConstants...)
	p.managerOptions.ConstantEditors = append(p.managerOptions.ConstantEditors, getCGroupWriteConstants())

	// if we are using tracepoints to probe syscall exits, i.e. if we are using an old kernel version (< 4.12)
	// we need to use raw_syscall tracepoints for exits, as syscall are not trace when running an ia32 userspace
	// process
	if probes.ShouldUseSyscallExitTracepoints() {
		p.managerOptions.ConstantEditors = append(p.managerOptions.ConstantEditors,
			manager.ConstantEditor{
				Name:  "tracepoint_raw_syscall_fallback",
				Value: uint64(1),
			},
		)
	}

	// constants syscall monitor
	if p.config.SyscallMonitor {
		p.managerOptions.ConstantEditors = append(p.managerOptions.ConstantEditors, manager.ConstantEditor{
			Name:  "syscall_monitor",
			Value: uint64(1),
		})
	}

	// tail calls
	p.managerOptions.TailCallRouter = probes.AllTailRoutes(p.config.ERPCDentryResolutionEnabled)
	if !p.config.ERPCDentryResolutionEnabled {
		// exclude the programs that use the bpf_probe_write_user helper
		p.managerOptions.ExcludedFunctions = probes.AllBPFProbeWriteUserProgramFunctions()
	}

	resolvers, err := NewResolvers(config, p)
	if err != nil {
		return nil, err
	}
	p.resolvers = resolvers

	p.reOrderer = NewReOrderer(ctx,
		p.handleEvent,
		ExtractEventInfo,
		ReOrdererOpts{
			QueueSize:  10000,
			Rate:       50 * time.Millisecond,
			Retention:  5,
			MetricRate: 5 * time.Second,
		})

	p.scrubber = pconfig.NewDefaultDataScrubber()
	p.scrubber.AddCustomSensitiveWords(config.CustomSensitiveWords)

	p.event = NewEvent(p.resolvers, p.scrubber)

	eventZero.resolvers = p.resolvers
	eventZero.scrubber = p.scrubber

	return p, nil
}

func (p *Probe) ensureConfigDefaults() {
	// enable runtime compiled constants on COS by default
	if !p.config.RuntimeCompiledConstantsIsSet && p.kernelVersion.IsCOSKernel() {
		p.config.EnableRuntimeCompiledConstants = true
	}
}

// GetOffsetConstants returns the offsets and struct sizes constants
func GetOffsetConstants(config *config.Config, probe *Probe) (map[string]uint64, error) {
	constantFetcher := constantfetch.ComposeConstantFetchers(constantfetch.GetAvailableConstantFetchers(config, probe.kernelVersion, probe.statsdClient))
	kv, err := probe.GetKernelVersion()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch probe kernel version: %w", err)
	}
	return GetOffsetConstantsFromFetcher(constantFetcher, kv)
}

// GetOffsetConstantsFromFetcher returns the offsets and struct sizes constants, from a constant fetcher
func GetOffsetConstantsFromFetcher(constantFetcher constantfetch.ConstantFetcher, kv *kernel.Version) (map[string]uint64, error) {
	constantFetcher.AppendSizeofRequest("sizeof_inode", "struct inode", "linux/fs.h")
	constantFetcher.AppendOffsetofRequest("sb_magic_offset", "struct super_block", "s_magic", "linux/fs.h")
	constantFetcher.AppendOffsetofRequest("dentry_sb_offset", "struct dentry", "d_sb", "linux/dcache.h")
	constantFetcher.AppendOffsetofRequest("tty_offset", "struct signal_struct", "tty", "linux/sched/signal.h")
	constantFetcher.AppendOffsetofRequest("tty_name_offset", "struct tty_struct", "name", "linux/tty.h")
	constantFetcher.AppendOffsetofRequest("creds_uid_offset", "struct cred", "uid", "linux/cred.h")
	// bpf offsets
	constantFetcher.AppendOffsetofRequest("bpf_map_id_offset", "struct bpf_map", "id", "linux/bpf.h")
	if kv.Code != 0 && (kv.Code >= kernel.Kernel4_15 || kv.IsRH7Kernel()) {
		constantFetcher.AppendOffsetofRequest("bpf_map_name_offset", "struct bpf_map", "name", "linux/bpf.h")
	}
	constantFetcher.AppendOffsetofRequest("bpf_map_type_offset", "struct bpf_map", "map_type", "linux/bpf.h")
	constantFetcher.AppendOffsetofRequest("bpf_prog_aux_id_offset", "struct bpf_prog_aux", "id", "linux/bpf.h")
	if kv.Code != 0 && (kv.Code >= kernel.Kernel4_15 || kv.IsRH7Kernel()) {
		constantFetcher.AppendOffsetofRequest("bpf_prog_aux_name_offset", "struct bpf_prog_aux", "name", "linux/bpf.h")
	}
	constantFetcher.AppendOffsetofRequest("bpf_prog_tag_offset", "struct bpf_prog", "tag", "linux/filter.h")
	constantFetcher.AppendOffsetofRequest("bpf_prog_aux_offset", "struct bpf_prog", "aux", "linux/filter.h")
	constantFetcher.AppendOffsetofRequest("bpf_prog_type_offset", "struct bpf_prog", "type", "linux/filter.h")

	if kv.Code != 0 && (kv.Code > kernel.Kernel4_16 || kv.IsSLES12Kernel() || kv.IsSLES15Kernel()) {
		constantFetcher.AppendOffsetofRequest("bpf_prog_attach_type_offset", "struct bpf_prog", "expected_attach_type", "linux/filter.h")
	}
	// namespace nr offsets
	constantFetcher.AppendOffsetofRequest("pid_level_offset", "struct pid", "level", "linux/pid.h")
	constantFetcher.AppendOffsetofRequest("pid_numbers_offset", "struct pid", "numbers", "linux/pid.h")
	constantFetcher.AppendSizeofRequest("sizeof_upid", "struct upid", "linux/pid.h")

	// splice event
	constantFetcher.AppendOffsetofRequest("pipe_inode_info_bufs_offset", "struct pipe_inode_info", "bufs", "linux/pipe_fs_i.h")

	return constantFetcher.FinishAndGetResults()
}
