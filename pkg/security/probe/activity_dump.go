// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux
// +build linux

package probe

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/DataDog/gopsutil/process"
	"github.com/pkg/errors"

	"github.com/DataDog/datadog-agent/pkg/security/api"
	seclog "github.com/DataDog/datadog-agent/pkg/security/log"
	"github.com/DataDog/datadog-agent/pkg/security/metrics"
	"github.com/DataDog/datadog-agent/pkg/security/secl/compiler/eval"
	"github.com/DataDog/datadog-agent/pkg/security/secl/model"
	"github.com/DataDog/datadog-agent/pkg/security/utils"
)

// NodeGenerationType is used to indicate if a node was generated by a runtime or snapshot event
type NodeGenerationType string

var (
	// Runtime is a node that was added at runtime
	Runtime NodeGenerationType = "runtime"
	// Snapshot is a node that was added during the snapshot
	Snapshot NodeGenerationType = "snapshot"
)

// ActivityDump holds the activity tree for the workload defined by the provided list of tags
type ActivityDump struct {
	sync.Mutex
	adm *ActivityDumpManager

	processedCount     map[model.EventType]*uint64
	addedRuntimeCount  map[model.EventType]*uint64
	addedSnapshotCount map[model.EventType]*uint64

	ProcessActivityTree []*ProcessActivityNode          `json:"tree"`
	CookiesNode         map[uint32]*ProcessActivityNode `json:"-"`
	DifferentiateArgs   bool
	WithGraph           bool

	OutputDirectory string `json:"-"`
	OutputFile      string `json:"-"`
	outputFile      *os.File
	GraphFile       string `json:"-"`
	graphFile       *os.File

	Comm        string        `json:"comm,omitempty"`
	ContainerID string        `json:"container_id,omitempty"`
	Tags        []string      `json:"tags,omitempty"`
	Start       time.Time     `json:"start"`
	Timeout     time.Duration `json:"duration"`
	End         time.Time     `json:"end"`
	timeoutRaw  int64
}

// WithDumpOption can be used to configure an ActivityDump
type WithDumpOption func(ad *ActivityDump)

// NewActivityDump returns a new instance of an ActivityDump
func NewActivityDump(adm *ActivityDumpManager, options ...WithDumpOption) (*ActivityDump, error) {
	var err error

	ad := ActivityDump{
		CookiesNode:        make(map[uint32]*ProcessActivityNode),
		Start:              time.Now(),
		adm:                adm,
		processedCount:     make(map[model.EventType]*uint64),
		addedRuntimeCount:  make(map[model.EventType]*uint64),
		addedSnapshotCount: make(map[model.EventType]*uint64),
	}

	for _, option := range options {
		option(&ad)
	}

	// generate counters
	for i := model.EventType(0); i < model.MaxEventType; i++ {
		processed := uint64(0)
		ad.processedCount[i] = &processed
		runtime := uint64(0)
		ad.addedRuntimeCount[i] = &runtime
		snapshot := uint64(0)
		ad.addedSnapshotCount[i] = &snapshot
	}

	if len(ad.OutputDirectory) > 0 {
		// generate random output file
		_ = os.MkdirAll(ad.OutputDirectory, 0400)
		ad.outputFile, err = os.CreateTemp(ad.OutputDirectory, "activity-dump-*.json")
		if err != nil {
			return nil, err
		}

		if err = os.Chmod(ad.outputFile.Name(), 0400); err != nil {
			ad.close()
			return nil, err
		}
		ad.OutputFile = ad.outputFile.Name()

		// generate random graph file
		if ad.WithGraph {
			ad.graphFile, err = os.CreateTemp(ad.OutputDirectory, "graph-dump-*.dot")
			if err != nil {
				ad.close()
				return nil, err
			}

			if err = os.Chmod(ad.graphFile.Name(), 0400); err != nil {
				ad.close()
				return nil, err
			}
			ad.GraphFile = ad.graphFile.Name()
		}
	}
	return &ad, nil
}

// Close closes all file descriptors of the activity dump
func (ad *ActivityDump) Close() {
	ad.Lock()
	defer ad.Unlock()
	ad.close()
}

// close thread unsafe version of Close
func (ad *ActivityDump) close() {
	if ad.graphFile != nil {
		_ = ad.graphFile.Close()
	}
	if ad.outputFile != nil {
		_ = ad.outputFile.Close()
	}
}

// GetTimeoutRawTimestamp returns the timeout timestamp of the current activity dump as a monolitic kernel timestamp
func (ad *ActivityDump) GetTimeoutRawTimestamp() int64 {
	if ad.timeoutRaw == 0 {
		ad.timeoutRaw = ad.adm.probe.resolvers.TimeResolver.ComputeMonotonicTimestamp(ad.Start.Add(ad.Timeout))
	}
	return ad.timeoutRaw
}

// UpdateTracedPidTimeout updates the timeout of a traced pid in kernel space
func (ad *ActivityDump) UpdateTracedPidTimeout(pid uint32) {
	// start by looking up any existing entry
	var timeout int64
	_ = ad.adm.tracedPIDsMap.Lookup(pid, &timeout)
	if timeout < ad.GetTimeoutRawTimestamp() {
		_ = ad.adm.tracedPIDsMap.Put(pid, ad.GetTimeoutRawTimestamp())
	}
}

// CommMatches returns true if the ActivityDump comm matches the provided comm
func (ad *ActivityDump) CommMatches(comm string) bool {
	return ad.Comm == comm
}

// ContainerIDMatches returns true if the ActivityDump container ID matches the provided container ID
func (ad *ActivityDump) ContainerIDMatches(containerID string) bool {
	return ad.ContainerID == containerID
}

// Matches returns true if the provided list of tags and / or the provided comm match the current ActivityDump
func (ad *ActivityDump) Matches(entry *model.ProcessCacheEntry) bool {
	if entry == nil {
		return false
	}

	if len(ad.ContainerID) > 0 {
		if !ad.ContainerIDMatches(entry.ContainerID) {
			return false
		}
	}

	if len(ad.Comm) > 0 {
		if !ad.CommMatches(entry.Comm) {
			return false
		}
	}

	return true
}

// Done stops an active dump
func (ad *ActivityDump) Done() {
	ad.Lock()
	defer ad.Unlock()

	// remove comm from kernel space
	if len(ad.Comm) > 0 {
		commB := make([]byte, 16)
		copy(commB, ad.Comm)
		err := ad.adm.tracedCommsMap.Delete(commB)
		if err != nil {
			seclog.Debugf("couldn't delete activity dump filter comm(%s): %v", ad.Comm, err)
		}
	}

	// remove container ID from kernel space
	if len(ad.ContainerID) > 0 {
		containerIDB := make([]byte, model.ContainerIDLen)
		copy(containerIDB, ad.ContainerID)
		err := ad.adm.tracedCgroupsMap.Delete(containerIDB)
		if err != nil {
			seclog.Debugf("couldn't delete activity dump filter containerID(%s): %v", ad.ContainerID, err)
		}
	}

	ad.End = time.Now()
	ad.dump()

	if ad.graphFile != nil {
		err := ad.generateGraph()
		if err != nil {
			seclog.Errorf("couldn't generate activity graph: %v", err)
		} else {
			seclog.Infof("activity graph for [%s] written at: %s", ad.GetSelectorStr(), ad.GraphFile)
		}
	}

	ad.close()

	// release all shared resources
	for _, p := range ad.ProcessActivityTree {
		p.recursiveRelease()
	}
}

func (ad *ActivityDump) dump() {
	if ad.outputFile == nil {
		return
	}

	raw, err := json.Marshal(ad)
	if err != nil {
		seclog.Errorf("couldn't marshal ActivityDump: %v\n", err)
		return
	}
	n, err := ad.outputFile.Write(raw)
	if err != nil {
		seclog.Errorf("couldn't write ActivityDump: %v\n", err)
		return
	}

	// send dump size
	if n > 0 {
		var tags []string
		if err = ad.adm.probe.statsdClient.Gauge(metrics.MetricActivityDumpSizeInBytes, float64(n), tags, 1.0); err != nil {
			seclog.Warnf("couldn't send %s metric: %v", metrics.MetricActivityDumpSizeInBytes, err)
		}
	}
	seclog.Infof("activity dump for [%s] written at: %s", ad.GetSelectorStr(), ad.OutputFile)
}

// nolint: unused
func (ad *ActivityDump) debug() {
	for _, root := range ad.ProcessActivityTree {
		root.debug("")
	}
}

// Insert inserts the provided event in the active ActivityDump. This function returns true if a new entry was added,
// false if the event was dropped.
func (ad *ActivityDump) Insert(event *Event) (newEntry bool) {
	ad.Lock()
	defer ad.Unlock()

	// ignore fork events for now
	if event.GetEventType() == model.ForkEventType {
		return false
	}

	// metrics
	defer func() {
		if newEntry {
			// this doesn't count the exec events which are counted separately
			atomic.AddUint64(ad.addedRuntimeCount[event.GetEventType()], 1)
		}
	}()

	// find the node where the event should be inserted
	node := ad.FindOrCreateProcessActivityNode(event.ResolveProcessCacheEntry(), Runtime)
	if node == nil {
		// a process node couldn't be found for the provided event as it doesn't match the ActivityDump query
		return false
	}

	// check if this event type is traced
	var traced bool
	for _, evtType := range ad.adm.tracedEventTypes {
		if evtType == event.GetEventType() {
			traced = true
		}
	}
	if !traced {
		return false
	}

	// resolve fields
	event.ResolveFields()

	// the count of processed events is the count of events that matched the activity dump selector = the events for
	// which we successfully found a process activity node
	atomic.AddUint64(ad.processedCount[event.GetEventType()], 1)

	// insert the event based on its type
	switch event.GetEventType() {
	case model.FileOpenEventType:
		return node.InsertFileEvent(&event.Open.File, event, Runtime)
	}
	return false
}

// FindOrCreateProcessActivityNode finds or a create a new process activity node in the activity dump if the entry
// matches the activity dump selector.
func (ad *ActivityDump) FindOrCreateProcessActivityNode(entry *model.ProcessCacheEntry, generationType NodeGenerationType) *ProcessActivityNode {
	var node *ProcessActivityNode

	if entry == nil {
		return node
	}

	// look for a ProcessActivityNode by process cookie
	if entry.Cookie > 0 {
		var found bool
		node, found = ad.CookiesNode[entry.Cookie]
		if found {
			return node
		}
	}

	// find or create a ProcessActivityNode for the parent of the input ProcessCacheEntry. If the parent is a fork entry,
	// jump immediately to the next ancestor.
	parentNode := ad.FindOrCreateProcessActivityNode(entry.GetNextAncestorNoFork(), Snapshot)

	// if parentNode is nil, the parent of the current node is out of tree (either because the parent is null, or it
	// doesn't match the dump tags).
	if parentNode == nil {

		// since the parent of the current entry wasn't inserted, we need to know if the current entry needs to be inserted.
		if !ad.Matches(entry) {
			return node
		}

		// go through the root nodes and check if one of them matches the input ProcessCacheEntry:
		for _, root := range ad.ProcessActivityTree {
			if root.Matches(entry, ad.DifferentiateArgs, ad.adm.probe.resolvers) {
				return root
			}
		}
		// if it doesn't, create a new ProcessActivityNode for the input ProcessCacheEntry
		node = NewProcessActivityNode(entry, generationType)
		// insert in the list of root entries
		ad.ProcessActivityTree = append(ad.ProcessActivityTree, node)

	} else {

		// if parentNode wasn't nil, then (at least) the parent is part of the activity dump. This means that we need
		// to add the current entry no matter if it matches the selector or not. Go through the root children of the
		// parent node and check if one of them matches the input ProcessCacheEntry.
		for _, child := range parentNode.Children {
			if child.Matches(entry, ad.DifferentiateArgs, ad.adm.probe.resolvers) {
				return child
			}
		}

		// if none of them matched, create a new ProcessActivityNode for the input processCacheEntry
		node = NewProcessActivityNode(entry, generationType)
		// insert in the list of root entries
		parentNode.Children = append(parentNode.Children, node)
	}

	// insert new cookie shortcut
	if entry.Cookie > 0 {
		ad.CookiesNode[entry.Cookie] = node
	}

	// count new entry
	switch generationType {
	case Runtime:
		atomic.AddUint64(ad.addedRuntimeCount[model.ExecEventType], 1)
	case Snapshot:
		atomic.AddUint64(ad.addedSnapshotCount[model.ExecEventType], 1)
	}

	// set the pid of the input ProcessCacheEntry as traced
	ad.UpdateTracedPidTimeout(entry.Pid)

	return node
}

// Profile holds the list of rules generated from an activity dump
type Profile struct {
	Name     string
	Selector string
	Rules    []ProfileRule
}

// ProfileRule contains the data required to generate a rule
type ProfileRule struct {
	ID         string
	Expression string
}

// NewProfileRule returns a new ProfileRule
func NewProfileRule(expression string, ruleIDPrefix string) ProfileRule {
	return ProfileRule{
		ID:         ruleIDPrefix + "_" + eval.RandString(5),
		Expression: expression,
	}
}

func (ad *ActivityDump) generateFIMRules(file *FileActivityNode, activityNode *ProcessActivityNode, ancestors []*ProcessActivityNode, ruleIDPrefix string) []ProfileRule {
	var rules []ProfileRule

	if file.File == nil {
		return rules
	}

	if file.Open != nil {
		rule := NewProfileRule(fmt.Sprintf(
			"open.file.path == \"%s\" && open.file.in_upper_layer == %v && open.file.uid == %d && open.file.gid == %d",
			file.File.PathnameStr,
			file.File.InUpperLayer,
			file.File.UID,
			file.File.GID),
			ruleIDPrefix,
		)
		rule.Expression += fmt.Sprintf(" && process.file.path == \"%s\"", activityNode.Process.PathnameStr)
		for _, parent := range ancestors {
			rule.Expression += fmt.Sprintf(" && process.ancestors.file.path == \"%s\"", parent.Process.PathnameStr)
		}
		rules = append(rules, rule)
	}

	for _, child := range file.Children {
		childrenRules := ad.generateFIMRules(child, activityNode, ancestors, ruleIDPrefix)
		rules = append(rules, childrenRules...)
	}

	return rules
}

func (ad *ActivityDump) generateRules(node *ProcessActivityNode, ancestors []*ProcessActivityNode, ruleIDPrefix string) []ProfileRule {
	var rules []ProfileRule

	// add exec rule
	rule := NewProfileRule(fmt.Sprintf(
		"exec.file.path == \"%s\" && process.uid == %d && process.gid == %d && process.cap_effective == %d && process.cap_permitted == %d",
		node.Process.PathnameStr,
		node.Process.UID,
		node.Process.GID,
		node.Process.CapEffective,
		node.Process.CapPermitted),
		ruleIDPrefix,
	)
	for _, parent := range ancestors {
		rule.Expression += fmt.Sprintf(" && process.ancestors.file.path == \"%s\"", parent.Process.PathnameStr)
	}
	rules = append(rules, rule)

	// add FIM rules
	for _, file := range node.Files {
		fimRules := ad.generateFIMRules(file, node, ancestors, ruleIDPrefix)
		rules = append(rules, fimRules...)
	}

	// add children rules recursively
	newAncestors := append([]*ProcessActivityNode{node}, ancestors...)
	for _, child := range node.Children {
		childrenRules := ad.generateRules(child, newAncestors, ruleIDPrefix)
		rules = append(rules, childrenRules...)
	}

	return rules
}

// GenerateProfileData generates a Profile from the activity dump
func (ad *ActivityDump) GenerateProfileData() Profile {
	p := Profile{
		Name: "profile_" + eval.RandString(5),
	}

	// generate selector
	if len(ad.Comm) > 0 {
		p.Selector = fmt.Sprintf("process.comm = \"%s\"", ad.Comm)
	}

	// Add rules
	for _, node := range ad.ProcessActivityTree {
		rules := ad.generateRules(node, nil, p.Name)
		p.Rules = append(p.Rules, rules...)
	}

	return p
}

// GetSelectorStr returns a string representation of the profile selector
func (ad *ActivityDump) GetSelectorStr() string {
	if len(ad.Tags) > 0 {
		return strings.Join(ad.Tags, ",")
	}
	if len(ad.ContainerID) > 0 {
		return fmt.Sprintf("container_id:%s", ad.ContainerID)
	}
	if len(ad.Comm) > 0 {
		return fmt.Sprintf("comm:%s", ad.Comm)
	}
	return "empty_selector"
}

// SendStats sends activity dump stats
func (ad *ActivityDump) SendStats() error {
	for evtType, count := range ad.processedCount {
		tags := []string{fmt.Sprintf("event_type:%s", evtType)}
		if value := atomic.SwapUint64(count, 0); value > 0 {
			if err := ad.adm.probe.statsdClient.Count(metrics.MetricActivityDumpEventProcessed, int64(value), tags, 1.0); err != nil {
				return errors.Wrapf(err, "couldn't send %s metric", metrics.MetricActivityDumpEventProcessed)
			}
		}
	}

	for evtType, count := range ad.addedRuntimeCount {
		tags := []string{fmt.Sprintf("event_type:%s", evtType), fmt.Sprintf("generation_type:%s", Runtime)}
		if value := atomic.SwapUint64(count, 0); value > 0 {
			if err := ad.adm.probe.statsdClient.Count(metrics.MetricActivityDumpEventAdded, int64(value), tags, 1.0); err != nil {
				return errors.Wrapf(err, "couldn't send %s metric", metrics.MetricActivityDumpEventAdded)
			}
		}
	}

	for evtType, count := range ad.addedSnapshotCount {
		tags := []string{fmt.Sprintf("event_type:%s", evtType), fmt.Sprintf("generation_type:%s", Snapshot)}
		if value := atomic.SwapUint64(count, 0); value > 0 {
			if err := ad.adm.probe.statsdClient.Count(metrics.MetricActivityDumpEventAdded, int64(value), tags, 1.0); err != nil {
				return errors.Wrapf(err, "couldn't send %s metric", metrics.MetricActivityDumpEventAdded)
			}
		}
	}

	return nil
}

// Snapshot snapshots the processes in the activity dump to capture all the
func (ad *ActivityDump) Snapshot() error {
	ad.Lock()
	defer ad.Unlock()

	for _, pan := range ad.ProcessActivityTree {
		if err := pan.snapshot(ad); err != nil {
			return err
		}
		// iterate slowly
		time.Sleep(50 * time.Millisecond)
	}

	// try to resolve the tags now
	_ = ad.resolveTags()
	return nil
}

// ResolveTags tries to resolve the activity dump tags
func (ad *ActivityDump) ResolveTags() error {
	ad.Lock()
	defer ad.Unlock()
	return ad.resolveTags()
}

// resolveTags thread unsafe version ot ResolveTags
func (ad *ActivityDump) resolveTags() error {
	if len(ad.Tags) > 0 {
		return nil
	}

	var err error
	ad.Tags, err = ad.adm.probe.resolvers.TagsResolver.ResolveWithErr(ad.ContainerID)
	if err != nil {
		return fmt.Errorf("failed to resolve %s: %w", ad.ContainerID, err)
	}
	return nil
}

// ToSecurityActivityDumpMessage returns a pointer to a SecurityActivityDumpMessage struct populated with current dump
// information.
func (ad *ActivityDump) ToSecurityActivityDumpMessage() *api.SecurityActivityDumpMessage {
	return &api.SecurityActivityDumpMessage{
		OutputFilename:    ad.OutputFile,
		GraphFilename:     ad.GraphFile,
		Comm:              ad.Comm,
		ContainerID:       ad.ContainerID,
		Tags:              ad.Tags,
		WithGraph:         ad.WithGraph,
		DifferentiateArgs: ad.DifferentiateArgs,
		Timeout:           ad.Timeout.String(),
		Start:             ad.Start.String(),
		Left:              ad.Start.Add(ad.Timeout).Sub(time.Now()).String(),
	}
}

// ProcessActivityNode holds the activity of a process
type ProcessActivityNode struct {
	id             string
	Process        model.Process      `json:"process"`
	GenerationType NodeGenerationType `json:"creation_type"`

	Files    map[string]*FileActivityNode `json:"files"`
	Children []*ProcessActivityNode       `json:"children"`
}

// GetID returns a unique ID to identify the current node
func (pan *ProcessActivityNode) GetID() string {
	if len(pan.id) == 0 {
		pan.id = eval.RandString(5)
	}
	return pan.id
}

// NewProcessActivityNode returns a new ProcessActivityNode instance
func NewProcessActivityNode(entry *model.ProcessCacheEntry, generationType NodeGenerationType) *ProcessActivityNode {
	pan := ProcessActivityNode{
		Process:        entry.Process,
		GenerationType: generationType,
		Files:          make(map[string]*FileActivityNode),
	}
	_ = pan.GetID()
	pan.retain()
	return &pan
}

// nolint: unused
func (pan *ProcessActivityNode) debug(prefix string) {
	fmt.Printf("%s- process: %s\n", prefix, pan.Process.PathnameStr)
	if len(pan.Files) > 0 {
		fmt.Printf("%s  files:\n", prefix)
		for _, f := range pan.Files {
			f.debug(fmt.Sprintf("%s\t-", prefix))
		}
	}
	if len(pan.Children) > 0 {
		fmt.Printf("%s  children:\n", prefix)
		for _, child := range pan.Children {
			child.debug(prefix + "\t")
		}
	}
}

func (pan *ProcessActivityNode) retain() {
	if pan.Process.ArgsEntry != nil && pan.Process.ArgsEntry.ArgsEnvsCacheEntry != nil {
		pan.Process.ArgsEntry.ArgsEnvsCacheEntry.Retain()
	}
	if pan.Process.EnvsEntry != nil && pan.Process.EnvsEntry.ArgsEnvsCacheEntry != nil {
		pan.Process.EnvsEntry.ArgsEnvsCacheEntry.Retain()
	}
}

func (pan *ProcessActivityNode) release() {
	if pan.Process.ArgsEntry != nil && pan.Process.ArgsEntry.ArgsEnvsCacheEntry != nil {
		pan.Process.ArgsEntry.ArgsEnvsCacheEntry.Release()
	}
	if pan.Process.EnvsEntry != nil && pan.Process.EnvsEntry.ArgsEnvsCacheEntry != nil {
		pan.Process.EnvsEntry.ArgsEnvsCacheEntry.Release()
	}
}

func (pan *ProcessActivityNode) recursiveRelease() {
	pan.release()
	for _, child := range pan.Children {
		child.recursiveRelease()
	}
}

// Matches return true if the process fields used to generate the dump are identical with the provided ProcessCacheEntry
func (pan *ProcessActivityNode) Matches(entry *model.ProcessCacheEntry, matchArgs bool, resolvers *Resolvers) bool {

	if pan.Process.Comm == entry.Comm && pan.Process.PathnameStr == entry.PathnameStr &&
		pan.Process.Credentials == entry.Credentials {

		if matchArgs {

			panArgs, _ := resolvers.ProcessResolver.GetProcessArgv(&pan.Process)
			entryArgs, _ := resolvers.ProcessResolver.GetProcessArgv(&entry.Process)
			if len(panArgs) != len(entryArgs) {
				return false
			}

			var found bool
			for _, arg1 := range panArgs {
				found = false
				for _, arg2 := range entryArgs {
					if arg1 == arg2 {
						found = true
						break
					}
				}
				if !found {
					return false
				}
			}
			return true
		}

		return true
	}
	return false
}

func extractFirstParent(path string) (string, int) {
	if len(path) == 0 {
		return "", 0
	}
	if path == "/" {
		return "", 0
	}

	var add int
	if path[0] == '/' {
		path = path[1:]
		add = 1
	}

	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			return path[0:i], i + add
		}
	}

	return path, len(path) + add
}

// InsertFileEvent inserts the provided file event in the current node. This function returns true if a new entry was
// added, false if the event was dropped.
func (pan *ProcessActivityNode) InsertFileEvent(fileEvent *model.FileEvent, event *Event, generationType NodeGenerationType) bool {
	parent, nextParentIndex := extractFirstParent(event.ResolveFilePath(fileEvent))
	if nextParentIndex == 0 {
		return false
	}

	// TODO: look for patterns / merge algo

	child, ok := pan.Files[parent]
	if ok {
		return child.InsertFileEvent(fileEvent, event, fileEvent.PathnameStr[nextParentIndex:], generationType)
	}

	// create new child
	if len(fileEvent.PathnameStr) <= nextParentIndex+1 {
		pan.Files[parent] = NewFileActivityNode(fileEvent, event, parent, generationType)
	} else {
		child := NewFileActivityNode(nil, nil, parent, generationType)
		child.InsertFileEvent(fileEvent, event, fileEvent.PathnameStr[nextParentIndex:], generationType)
		pan.Files[parent] = child
	}
	return true
}

// snapshot uses procfs to retrieve information about the current process
func (pan *ProcessActivityNode) snapshot(ad *ActivityDump) error {
	// call snapshot for all the children of the current node
	for _, child := range pan.Children {
		if err := child.snapshot(ad); err != nil {
			return err
		}
		// iterate slowly
		time.Sleep(50 * time.Millisecond)
	}

	// snapshot the current process
	p, err := process.NewProcess(int32(pan.Process.Pid))
	if err != nil {
		// the process doesn't exist anymore, ignore
		return nil
	}

	for _, eventType := range ad.adm.tracedEventTypes {
		switch eventType {
		case model.FileOpenEventType:
			if err = pan.snapshotFiles(p, ad); err != nil {
				return err
			}
		}
	}
	return nil
}

func (pan *ProcessActivityNode) snapshotFiles(p *process.Process, ad *ActivityDump) error {
	// list the files opened by the process
	fileFDs, err := p.OpenFiles()
	if err != nil {
		return err
	}

	var files []string
	for _, fd := range fileFDs {
		files = append(files, fd.Path)
	}

	// list the mmaped files of the process
	memoryMaps, err := p.MemoryMaps(false)
	if err != nil {
		return err
	}

	for _, mm := range *memoryMaps {
		if mm.Path != pan.Process.PathnameStr {
			files = append(files, mm.Path)
		}
	}

	// insert files
	var fileinfo os.FileInfo
	var resolvedPath string
	for _, f := range files {
		if len(f) == 0 {
			continue
		}

		// fetch the file user, group and mode
		fullPath := filepath.Join(utils.RootPath(int32(pan.Process.Pid)), f)
		fileinfo, err = os.Stat(fullPath)
		if err != nil {
			continue
		}
		stat, ok := fileinfo.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		evt := NewEvent(ad.adm.probe.resolvers, ad.adm.probe.scrubber)
		evt.Event.Type = uint64(model.FileOpenEventType)

		resolvedPath, err = filepath.EvalSymlinks(f)
		if err != nil {
			evt.Open.File.PathnameStr = resolvedPath
		} else {
			evt.Open.File.PathnameStr = f
		}
		evt.Open.File.BasenameStr = path.Base(evt.Open.File.PathnameStr)
		evt.Open.File.FileFields.Mode = uint16(stat.Mode)
		evt.Open.File.FileFields.Inode = stat.Ino
		evt.Open.File.FileFields.UID = stat.Uid
		evt.Open.File.FileFields.GID = stat.Gid
		evt.Open.File.FileFields.MTime = uint64(ad.adm.probe.resolvers.TimeResolver.ComputeMonotonicTimestamp(time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)))
		evt.Open.File.FileFields.CTime = uint64(ad.adm.probe.resolvers.TimeResolver.ComputeMonotonicTimestamp(time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec)))

		evt.Open.File.Mode = evt.Open.File.FileFields.Mode
		// TODO: add open flags by parsing `/proc/[pid]/fdinfo/fd` + O_RDONLY|O_CLOEXEC for the shared libs

		if pan.InsertFileEvent(&evt.Open.File, evt, Snapshot) {
			// count this new entry
			atomic.AddUint64(ad.addedSnapshotCount[model.FileOpenEventType], 1)
		}
	}
	return nil
}

// FileActivityNode holds a tree representation of a list of files
type FileActivityNode struct {
	id             string
	Name           string             `json:"name"`
	File           *model.FileEvent   `json:"file,omitempty"`
	GenerationType NodeGenerationType `json:"generation_type"`
	FirstSeen      time.Time          `json:"first_seen,omitempty"`

	Open *OpenNode `json:"open,omitempty"`

	Children map[string]*FileActivityNode `json:"children"`
}

// GetID returns a unique ID to identify the current node
func (fan *FileActivityNode) GetID() string {
	if len(fan.id) == 0 {
		fan.id = eval.RandString(5)
	}
	return fan.id
}

// OpenNode contains the relevant fields of an Open event on which we might want to write a profiling rule
type OpenNode struct {
	model.SyscallEvent
	Flags uint32
	Mode  uint32
}

// NewFileActivityNode returns a new FileActivityNode instance
func NewFileActivityNode(fileEvent *model.FileEvent, event *Event, name string, generationType NodeGenerationType) *FileActivityNode {
	fan := &FileActivityNode{
		Name:           name,
		GenerationType: generationType,
		Children:       make(map[string]*FileActivityNode),
	}
	_ = fan.GetID()
	if fileEvent != nil {
		fileEventTmp := *fileEvent
		fan.File = &fileEventTmp
	}
	fan.enrichFromEvent(event)
	return fan
}

func (fan *FileActivityNode) getNodeLabel() string {
	label := fan.Name
	if fan.Open != nil {
		label += " [open]"
	}
	return label
}

func (fan *FileActivityNode) enrichFromEvent(event *Event) {
	if event == nil {
		return
	}
	if fan.FirstSeen.IsZero() {
		fan.FirstSeen = event.ResolveEventTimestamp()
	}

	switch event.GetEventType() {
	case model.FileOpenEventType:
		fan.Open = &OpenNode{
			SyscallEvent: event.Open.SyscallEvent,
			Flags:        event.Open.Flags,
			Mode:         event.Open.Mode,
		}
	}
}

// InsertFileEvent inserts an event in a FileActivityNode. This function returns true if a new entry was added, false if
// the event was dropped.
func (fan *FileActivityNode) InsertFileEvent(fileEvent *model.FileEvent, event *Event, remainingPath string, generationType NodeGenerationType) bool {
	parent, nextParentIndex := extractFirstParent(remainingPath)
	if nextParentIndex == 0 {
		fan.enrichFromEvent(event)
		return false
	}

	// TODO: look for patterns / merge algo

	child, ok := fan.Children[parent]
	if ok {
		return child.InsertFileEvent(fileEvent, event, remainingPath[nextParentIndex:], generationType)
	}

	// create new child
	if len(remainingPath) <= nextParentIndex+1 {
		fan.Children[parent] = NewFileActivityNode(fileEvent, event, parent, generationType)
	} else {
		child := NewFileActivityNode(nil, nil, parent, generationType)
		child.InsertFileEvent(fileEvent, event, remainingPath[nextParentIndex:], generationType)
		fan.Children[parent] = child
	}
	return true
}

// nolint: unused
func (fan *FileActivityNode) debug(prefix string) {
	fmt.Printf("%s %s\n", prefix, fan.Name)
	for _, child := range fan.Children {
		child.debug("\t" + prefix)
	}
}
