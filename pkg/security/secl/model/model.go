// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:generate go run github.com/DataDog/datadog-agent/pkg/security/secl/compiler/generators/accessors -mock -output accessors.go
//go:generate go run github.com/DataDog/datadog-agent/pkg/security/secl/compiler/generators/accessors -tags linux -output ../../probe/accessors.go -doc ../../../../docs/cloud-workload-security/secl.json -fields-resolver ../../probe/fields_resolver.go

package model

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/pkg/errors"

	"github.com/DataDog/datadog-agent/pkg/security/secl/compiler/eval"
)

// Model describes the data model for the runtime security agent events
type Model struct{}

// NewEvent returns a new Event
func (m *Model) NewEvent() eval.Event {
	return &Event{}
}

// ValidateField validates the value of a field
func (m *Model) ValidateField(field eval.Field, fieldValue eval.FieldValue) error {
	// check that all path are absolute
	if strings.HasSuffix(field, "path") {

		// do not support regular expression on path, currently unable to support discarder for regex value
		if fieldValue.Type == eval.RegexpValueType {
			return fmt.Errorf("regexp not supported on path `%s`", field)
		}

		if value, ok := fieldValue.Value.(string); ok {
			errAbs := fmt.Errorf("invalid path `%s`, all the path have to be absolute", value)
			errDepth := fmt.Errorf("invalid path `%s`, path depths have to be shorter than %d", value, MaxPathDepth)
			errSegment := fmt.Errorf("invalid path `%s`, each segment of a path must be shorter than %d", value, MaxSegmentLength)

			if value != path.Clean(value) {
				return errAbs
			}

			if value == "*" {
				return errAbs
			}

			if !filepath.IsAbs(value) && len(value) > 0 && value[0] != '*' {
				return errAbs
			}

			if matched, err := regexp.Match(`^~`, []byte(value)); err != nil || matched {
				return errAbs
			}

			// check resolution limitations
			segments := strings.Split(value, "/")
			if len(segments) > MaxPathDepth {
				return errDepth
			}
			for _, segment := range segments {
				if segment == ".." {
					return errAbs
				}
				if len(segment) > MaxSegmentLength {
					return errSegment
				}
			}
		}
	}

	switch field {

	case "event.retval":
		if value := fieldValue.Value; value != -int(syscall.EPERM) && value != -int(syscall.EACCES) {
			return errors.New("return value can only be tested against EPERM or EACCES")
		}
	case "bpf.map.name", "bpf.prog.name":
		if value, ok := fieldValue.Value.(string); ok {
			if len(value) > MaxBpfObjName {
				return fmt.Errorf("the name provided in %s must be at most %d characters, len(\"%s\") = %d", field, MaxBpfObjName, value, len(value))
			}
		}
	}

	return nil
}

// ChmodEvent represents a chmod event
type ChmodEvent struct {
	SyscallEvent
	File FileEvent `field:"file"`
	Mode uint32    `field:"file.destination.mode" field:"file.destination.rights"` // New mode/rights of the chmod-ed file
}

// ChownEvent represents a chown event
type ChownEvent struct {
	SyscallEvent
	File  FileEvent `field:"file"`
	UID   int64     `field:"file.destination.uid"`                   // New UID of the chown-ed file's owner
	User  string    `field:"file.destination.user,ResolveChownUID"`  // New user of the chown-ed file's owner
	GID   int64     `field:"file.destination.gid"`                   // New GID of the chown-ed file's owner
	Group string    `field:"file.destination.group,ResolveChownGID"` // New group of the chown-ed file's owner
}

// ContainerContext holds the container context of an event
type ContainerContext struct {
	ID   string   `field:"id,ResolveContainerID"`          // ID of the container
	Tags []string `field:"tags,ResolveContainerTags:9999"` // Tags of the container
}

// Event represents an event sent from the kernel
// genaccessors
type Event struct {
	ID           string    `field:"-"`
	Type         uint64    `field:"-"`
	TimestampRaw uint64    `field:"-"`
	Timestamp    time.Time `field:"timestamp"` // Timestamp of the event

	ProcessContext   ProcessContext   `field:"process" event:"*"`
	SpanContext      SpanContext      `field:"-"`
	ContainerContext ContainerContext `field:"container"`

	Chmod       ChmodEvent    `field:"chmod" event:"chmod"`             // [7.27] [File] A file’s permissions were changed
	Chown       ChownEvent    `field:"chown" event:"chown"`             // [7.27] [File] A file’s owner was changed
	Open        OpenEvent     `field:"open" event:"open"`               // [7.27] [File] A file was opened
	Mkdir       MkdirEvent    `field:"mkdir" event:"mkdir"`             // [7.27] [File] A directory was created
	Rmdir       RmdirEvent    `field:"rmdir" event:"rmdir"`             // [7.27] [File] A directory was removed
	Rename      RenameEvent   `field:"rename" event:"rename"`           // [7.27] [File] A file/directory was renamed
	Unlink      UnlinkEvent   `field:"unlink" event:"unlink"`           // [7.27] [File] A file was deleted
	Utimes      UtimesEvent   `field:"utimes" event:"utimes"`           // [7.27] [File] Change file access/modification times
	Link        LinkEvent     `field:"link" event:"link"`               // [7.27] [File] Create a new name/alias for a file
	SetXAttr    SetXAttrEvent `field:"setxattr" event:"setxattr"`       // [7.27] [File] Set exteneded attributes
	RemoveXAttr SetXAttrEvent `field:"removexattr" event:"removexattr"` // [7.27] [File] Remove extended attributes
	Splice      SpliceEvent   `field:"splice" event:"splice"`           // [7.36] [File] A splice command was executed

	Exec   ExecEvent   `field:"exec" event:"exec"`     // [7.27] [Process] A process was executed or forked
	SetUID SetuidEvent `field:"setuid" event:"setuid"` // [7.27] [Process] A process changed its effective uid
	SetGID SetgidEvent `field:"setgid" event:"setgid"` // [7.27] [Process] A process changed its effective gid
	Capset CapsetEvent `field:"capset" event:"capset"` // [7.27] [Process] A process changed its capacity set
	Signal SignalEvent `field:"signal" event:"signal"` // [7.35] [Process] A signal was sent

	SELinux      SELinuxEvent      `field:"selinux" event:"selinux"`             // [7.30] [Kernel] An SELinux operation was run
	BPF          BPFEvent          `field:"bpf" event:"bpf"`                     // [7.33] [Kernel] A BPF command was executed
	PTrace       PTraceEvent       `field:"ptrace" event:"ptrace"`               // [7.35] [Kernel] A ptrace command was executed
	MMap         MMapEvent         `field:"mmap" event:"mmap"`                   // [7.35] [Kernel] A mmap command was executed
	MProtect     MProtectEvent     `field:"mprotect" event:"mprotect"`           // [7.35] [Kernel] A mprotect command was executed
	LoadModule   LoadModuleEvent   `field:"load_module" event:"load_module"`     // [7.35] [Kernel] A new kernel module was loaded
	UnloadModule UnloadModuleEvent `field:"unload_module" event:"unload_module"` // [7.35] [Kernel] A kernel module was deleted

	Mount            MountEvent            `field:"-"`
	Umount           UmountEvent           `field:"-"`
	InvalidateDentry InvalidateDentryEvent `field:"-"`
	ArgsEnvs         ArgsEnvsEvent         `field:"-"`
	MountReleased    MountReleasedEvent    `field:"-"`
}

// GetType returns the event type
func (e *Event) GetType() string {
	return EventType(e.Type).String()
}

// GetEventType returns the event type of the event
func (e *Event) GetEventType() EventType {
	return EventType(e.Type)
}

// GetTags returns the list of tags specific to this event
func (e *Event) GetTags() []string {
	tags := []string{"type:" + e.GetType()}

	// should already be resolved at this stage
	if len(e.ContainerContext.Tags) > 0 {
		tags = append(tags, e.ContainerContext.Tags...)
	}
	return tags
}

// GetPointer return an unsafe.Pointer of the Event
func (e *Event) GetPointer() unsafe.Pointer {
	return unsafe.Pointer(e)
}

// SetuidEvent represents a setuid event
type SetuidEvent struct {
	UID    uint32 `field:"uid"`                        // New UID of the process
	User   string `field:"user,ResolveSetuidUser"`     // New user of the process
	EUID   uint32 `field:"euid"`                       // New effective UID of the process
	EUser  string `field:"euser,ResolveSetuidEUser"`   // New effective user of the process
	FSUID  uint32 `field:"fsuid"`                      // New FileSystem UID of the process
	FSUser string `field:"fsuser,ResolveSetuidFSUser"` // New FileSystem user of the process
}

// SetgidEvent represents a setgid event
type SetgidEvent struct {
	GID     uint32 `field:"gid"`                          // New GID of the process
	Group   string `field:"group,ResolveSetgidGroup"`     // New group of the process
	EGID    uint32 `field:"egid"`                         // New effective GID of the process
	EGroup  string `field:"egroup,ResolveSetgidEGroup"`   // New effective group of the process
	FSGID   uint32 `field:"fsgid"`                        // New FileSystem GID of the process
	FSGroup string `field:"fsgroup,ResolveSetgidFSGroup"` // New FileSystem group of the process
}

// CapsetEvent represents a capset event
type CapsetEvent struct {
	CapEffective uint64 `field:"cap_effective"` // Effective capability set of the process
	CapPermitted uint64 `field:"cap_permitted"` // Permitted capability set of the process
}

// Credentials represents the kernel credentials of a process
type Credentials struct {
	UID   uint32 `field:"uid"`   // UID of the process
	GID   uint32 `field:"gid"`   // GID of the process
	User  string `field:"user"`  // User of the process
	Group string `field:"group"` // Group of the process

	EUID   uint32 `field:"euid"`   // Effective UID of the process
	EGID   uint32 `field:"egid"`   // Effective GID of the process
	EUser  string `field:"euser"`  // Effective user of the process
	EGroup string `field:"egroup"` // Effective group of the process

	FSUID   uint32 `field:"fsuid"`   // FileSystem-uid of the process
	FSGID   uint32 `field:"fsgid"`   // FileSystem-gid of the process
	FSUser  string `field:"fsuser"`  // FileSystem-user of the process
	FSGroup string `field:"fsgroup"` // FileSystem-group of the process

	CapEffective uint64 `field:"cap_effective"` // Effective capability set of the process
	CapPermitted uint64 `field:"cap_permitted"` // Permitted capability set of the process
}

// GetPathResolutionError returns the path resolution error as a string if there is one
func (e *Process) GetPathResolutionError() string {
	if e.PathResolutionError != nil {
		return e.PathResolutionError.Error()
	}
	return ""
}

// Process represents a process
type Process struct {
	// proc_cache_t
	FileFields FileFields `field:"file"`

	Pid uint32 `field:"pid"` // Process ID of the process (also called thread group ID)
	Tid uint32 `field:"tid"` // Thread ID of the thread

	PathnameStr         string `field:"file.path"`       // Path of the process executable
	BasenameStr         string `field:"file.name"`       // Basename of the path of the process executable
	Filesystem          string `field:"file.filesystem"` // FileSystem of the process executable
	PathResolutionError error  `field:"-"`

	ContainerID   string   `field:"container.id"` // Container ID
	ContainerTags []string `field:"-"`

	TTYName string `field:"tty_name"` // Name of the TTY associated with the process
	Comm    string `field:"comm"`     // Comm attribute of the process

	// pid_cache_t
	ForkTime time.Time `field:"-"`
	ExitTime time.Time `field:"-"`
	ExecTime time.Time `field:"-"`

	CreatedAt uint64 `field:"created_at,ResolveProcessCreatedAt"` // Timestamp of the creation of the process

	Cookie uint32 `field:"cookie"` // Cookie of the process
	PPid   uint32 `field:"ppid"`   // Parent process ID

	// credentials_t section of pid_cache_t
	Credentials

	ArgsID uint32 `field:"-"`
	EnvsID uint32 `field:"-"`

	ArgsEntry *ArgsEntry `field:"-"`
	EnvsEntry *EnvsEntry `field:"-"`

	// defined to generate accessors, ArgsTruncated and EnvsTruncated are used during by unmarshaller
	Argv0         string   `field:"argv0,ResolveProcessArgv0:100"`                                                                                                                                     // First argument of the process
	Args          string   `field:"args,ResolveProcessArgs:100"`                                                                                                                                       // Arguments of the process (as a string)
	Argv          []string `field:"argv,ResolveProcessArgv:100" field:"args_flags,ResolveProcessArgsFlags,,cacheless_resolution" field:"args_options,ResolveProcessArgsOptions,,cacheless_resolution"` // Arguments of the process (as an array)
	ArgsTruncated bool     `field:"args_truncated,ResolveProcessArgsTruncated"`                                                                                                                        // Indicator of arguments truncation
	Envs          []string `field:"envs,ResolveProcessEnvs:100"`                                                                                                                                       // Environment variable names of the process
	Envp          []string `field:"envp,ResolveProcessEnvp:100"`                                                                                                                                       // Environment variables of the process
	EnvsTruncated bool     `field:"envs_truncated,ResolveProcessEnvsTruncated"`                                                                                                                        // Indicator of environment variables truncation

	// cache version
	ScrubbedArgvResolved  bool           `field:"-"`
	ScrubbedArgv          []string       `field:"-"`
	ScrubbedArgsTruncated bool           `field:"-"`
	Variables             eval.Variables `field:"-"`
}

// SpanContext describes a span context
type SpanContext struct {
	SpanID  uint64 `field:"_"`
	TraceID uint64 `field:"_"`
}

// ExecEvent represents a exec event
type ExecEvent struct {
	Process
}

// FileFields holds the information required to identify a file
type FileFields struct {
	UID   uint32 `field:"uid"`                                                     // UID of the file's owner
	User  string `field:"user,ResolveFileFieldsUser"`                              // User of the file's owner
	GID   uint32 `field:"gid"`                                                     // GID of the file's owner
	Group string `field:"group,ResolveFileFieldsGroup"`                            // Group of the file's owner
	Mode  uint16 `field:"mode" field:"rights,ResolveRights,,cacheless_resolution"` // Mode/rights of the file
	CTime uint64 `field:"change_time"`                                             // Change time of the file
	MTime uint64 `field:"modification_time"`                                       // Modification time of the file

	MountID      uint32 `field:"mount_id"`                                     // Mount ID of the file
	Inode        uint64 `field:"inode"`                                        // Inode of the file
	InUpperLayer bool   `field:"in_upper_layer,ResolveFileFieldsInUpperLayer"` // Indicator of the file layer, in an OverlayFS for example

	NLink  uint32 `field:"-"`
	PathID uint32 `field:"-"`
	Flags  int32  `field:"-"`
}

// HasHardLinks returns whether the file has hardlink
func (f *FileFields) HasHardLinks() bool {
	return f.NLink > 1
}

// GetInLowerLayer returns whether a file is in a lower layer
func (f *FileFields) GetInLowerLayer() bool {
	return f.Flags&LowerLayer != 0
}

// GetInUpperLayer returns whether a file is in the upper layer
func (f *FileFields) GetInUpperLayer() bool {
	return f.Flags&UpperLayer != 0
}

// FileEvent is the common file event type
type FileEvent struct {
	FileFields
	PathnameStr string `field:"path,ResolveFilePath"`             // File's path
	BasenameStr string `field:"name,ResolveFileBasename"`         // File's basename
	Filesytem   string `field:"filesystem,ResolveFileFilesystem"` // File's filesystem

	PathResolutionError error `field:"-"`
}

// GetPathResolutionError returns the path resolution error as a string if there is one
func (e *FileEvent) GetPathResolutionError() string {
	if e.PathResolutionError != nil {
		return e.PathResolutionError.Error()
	}
	return ""
}

// InvalidateDentryEvent defines a invalidate dentry event
type InvalidateDentryEvent struct {
	Inode             uint64
	MountID           uint32
	DiscarderRevision uint32
}

// MountReleasedEvent defines a mount released event
type MountReleasedEvent struct {
	MountID           uint32
	DiscarderRevision uint32
}

// LinkEvent represents a link event
type LinkEvent struct {
	SyscallEvent
	Source FileEvent `field:"file"`
	Target FileEvent `field:"file.destination"`
}

// MkdirEvent represents a mkdir event
type MkdirEvent struct {
	SyscallEvent
	File FileEvent `field:"file"`
	Mode uint32    `field:"file.destination.mode" field:"file.destination.rights"` // Mode/rights of the new directory
}

// ArgsEnvsEvent defines a args/envs event
type ArgsEnvsEvent struct {
	ArgsEnvs
}

// MountEvent represents a mount event
type MountEvent struct {
	SyscallEvent
	MountID                       uint32
	GroupID                       uint32
	Device                        uint32
	ParentMountID                 uint32
	ParentInode                   uint64
	FSType                        string
	MountPointStr                 string
	MountPointPathResolutionError error
	RootMountID                   uint32
	RootInode                     uint64
	RootStr                       string
	RootPathResolutionError       error

	FSTypeRaw [16]byte
}

// GetFSType returns the filesystem type of the mountpoint
func (m *MountEvent) GetFSType() string {
	return m.FSType
}

// IsOverlayFS returns whether it is an overlay fs
func (m *MountEvent) IsOverlayFS() bool {
	return m.GetFSType() == "overlay"
}

// GetRootPathResolutionError returns the root path resolution error as a string if there is one
func (m *MountEvent) GetRootPathResolutionError() string {
	if m.RootPathResolutionError != nil {
		return m.RootPathResolutionError.Error()
	}
	return ""
}

// GetMountPointPathResolutionError returns the mount point path resolution error as a string if there is one
func (m *MountEvent) GetMountPointPathResolutionError() string {
	if m.MountPointPathResolutionError != nil {
		return m.MountPointPathResolutionError.Error()
	}
	return ""
}

// OpenEvent represents an open event
type OpenEvent struct {
	SyscallEvent
	File  FileEvent `field:"file"`
	Flags uint32    `field:"flags"`                 // Flags used when opening the file
	Mode  uint32    `field:"file.destination.mode"` // Mode of the created file
}

// SELinuxEventKind represents the event kind for SELinux events
type SELinuxEventKind uint32

const (
	// SELinuxBoolChangeEventKind represents SELinux boolean change events
	SELinuxBoolChangeEventKind SELinuxEventKind = iota
	// SELinuxStatusChangeEventKind represents SELinux status change events
	SELinuxStatusChangeEventKind
	// SELinuxBoolCommitEventKind represents SELinux boolean commit events
	SELinuxBoolCommitEventKind
)

// SELinuxEvent represents a selinux event
type SELinuxEvent struct {
	File            FileEvent        `field:"-"`
	EventKind       SELinuxEventKind `field:"-"`
	BoolName        string           `field:"bool.name,ResolveSELinuxBoolName"` // SELinux boolean name
	BoolChangeValue string           `field:"bool.state"`                       // SELinux boolean new value
	BoolCommitValue bool             `field:"bool_commit.state"`                // Indicator of a SELinux boolean commit operation
	EnforceStatus   string           `field:"enforce.status"`                   // SELinux enforcement status (one of "enforcing", "permissive", "disabled"")
}

var zeroProcessContext ProcessContext

// ProcessCacheEntry this struct holds process context kept in the process tree
type ProcessCacheEntry struct {
	ProcessContext

	refCount  uint64                     `field:"-"`
	onRelease func(_ *ProcessCacheEntry) `field:"-"`
	releaseCb func()                     `field:"-"`
}

// Reset the entry
func (pc *ProcessCacheEntry) Reset() {
	pc.ProcessContext = zeroProcessContext
	pc.refCount = 0
	pc.releaseCb = nil
}

// Retain increment ref counter
func (pc *ProcessCacheEntry) Retain() {
	pc.refCount++
}

// SetReleaseCallback set the callback called when the entry is released
func (pc *ProcessCacheEntry) SetReleaseCallback(callback func()) {
	pc.releaseCb = callback
}

// Release decrement and eventually release the entry
func (pc *ProcessCacheEntry) Release() {
	pc.refCount--
	if pc.refCount > 0 {
		return
	}

	if pc.onRelease != nil {
		pc.onRelease(pc)
	}

	if pc.releaseCb != nil {
		pc.releaseCb()
	}
}

// NewProcessCacheEntry returns a new process cache entry
func NewProcessCacheEntry(onRelease func(_ *ProcessCacheEntry)) *ProcessCacheEntry {
	return &ProcessCacheEntry{onRelease: onRelease}
}

// ProcessAncestorsIterator defines an iterator of ancestors
type ProcessAncestorsIterator struct {
	prev *ProcessCacheEntry
}

// Front returns the first element
func (it *ProcessAncestorsIterator) Front(ctx *eval.Context) unsafe.Pointer {
	if front := (*Event)(ctx.Object).ProcessContext.Ancestor; front != nil {
		it.prev = front
		return unsafe.Pointer(front)
	}

	return nil
}

// Next returns the next element
func (it *ProcessAncestorsIterator) Next() unsafe.Pointer {
	if next := it.prev.Ancestor; next != nil {
		it.prev = next
		return unsafe.Pointer(next)
	}

	return nil
}

// ProcessContext holds the process context of an event
type ProcessContext struct {
	Process

	Ancestor *ProcessCacheEntry `field:"ancestors,,ProcessAncestorsIterator"`
}

// RenameEvent represents a rename event
type RenameEvent struct {
	SyscallEvent
	Old               FileEvent `field:"file"`
	New               FileEvent `field:"file.destination"`
	DiscarderRevision uint32    `field:"-"`
}

// RmdirEvent represents a rmdir event
type RmdirEvent struct {
	SyscallEvent
	File              FileEvent `field:"file"`
	DiscarderRevision uint32    `field:"-"`
}

// SetXAttrEvent represents an extended attributes event
type SetXAttrEvent struct {
	SyscallEvent
	File      FileEvent `field:"file"`
	Namespace string    `field:"file.destination.namespace,ResolveXAttrNamespace"` // Namespace of the extended attribute
	Name      string    `field:"file.destination.name,ResolveXAttrName"`           // Name of the extended attribute

	NameRaw [200]byte `field:"-"`
}

// SyscallEvent contains common fields for all the event
type SyscallEvent struct {
	Retval int64 `field:"retval"` // Return value of the syscall
}

// UnlinkEvent represents an unlink event
type UnlinkEvent struct {
	SyscallEvent
	File              FileEvent `field:"file"`
	Flags             uint32    `field:"-"`
	DiscarderRevision uint32    `field:"-"`
}

// UmountEvent represents an umount event
type UmountEvent struct {
	SyscallEvent
	MountID uint32
}

// UtimesEvent represents a utime event
type UtimesEvent struct {
	SyscallEvent
	File  FileEvent `field:"file"`
	Atime time.Time `field:"-"`
	Mtime time.Time `field:"-"`
}

// BPFEvent represents a BPF event
type BPFEvent struct {
	SyscallEvent

	Map     BPFMap     `field:"map"`  // eBPF map involved in the BPF command
	Program BPFProgram `field:"prog"` // eBPF program involved in the BPF command
	Cmd     uint32     `field:"cmd"`  // BPF command name
}

// BPFMap represents a BPF map
type BPFMap struct {
	ID   uint32 `field:"-"`    // ID of the eBPF map
	Type uint32 `field:"type"` // Type of the eBPF map
	Name string `field:"name"` // Name of the eBPF map (added in 7.35)
}

// BPFProgram represents a BPF program
type BPFProgram struct {
	ID         uint32   `field:"-"`                      // ID of the eBPF program
	Type       uint32   `field:"type"`                   // Type of the eBPF program
	AttachType uint32   `field:"attach_type"`            // Attach type of the eBPF program
	Helpers    []uint32 `field:"helpers,ResolveHelpers"` // eBPF helpers used by the eBPF program (added in 7.35)
	Name       string   `field:"name"`                   // Name of the eBPF program (added in 7.35)
	Tag        string   `field:"tag"`                    // Hash (sha1) of the eBPF program (added in 7.35)
}

// PTraceEvent represents a ptrace event
type PTraceEvent struct {
	SyscallEvent

	Request uint32         `field:"request"` //  ptrace request
	PID     uint32         `field:"-"`
	Address uint64         `field:"-"`
	Tracee  ProcessContext `field:"tracee"` // process context of the tracee
}

// MMapEvent represents a mmap event
type MMapEvent struct {
	SyscallEvent

	File       FileEvent `field:"file"`
	Addr       uint64    `field:"-"`
	Offset     uint64    `field:"-"`
	Len        uint32    `field:"-"`
	Protection int       `field:"protection"` // memory segment protection
	Flags      int       `field:"flags"`      // memory segment flags
}

// MProtectEvent represents a mprotect event
type MProtectEvent struct {
	SyscallEvent

	VMStart       uint64 `field:"-"`
	VMEnd         uint64 `field:"-"`
	VMProtection  int    `field:"vm_protection"`  // initial memory segment protection
	ReqProtection int    `field:"req_protection"` // new memory segment protection
}

// LoadModuleEvent represents a load_module event
type LoadModuleEvent struct {
	SyscallEvent

	File             FileEvent `field:"file"`               // Path to the kernel module file
	LoadedFromMemory bool      `field:"loaded_from_memory"` // Indicates if the kernel module was loaded from memory
	Name             string    `field:"name"`               // Name of the new kernel module
}

// UnloadModuleEvent represents an unload_module event
type UnloadModuleEvent struct {
	SyscallEvent

	Name string `field:"name"` // Name of the kernel module that was deleted
}

// SignalEvent represents a signal event
type SignalEvent struct {
	SyscallEvent

	Type   uint32         `field:"type"`   // Signal type (ex: SIGHUP, SIGINT, SIGQUIT, etc)
	PID    uint32         `field:"pid"`    // Target PID
	Target ProcessContext `field:"target"` // Target process context
}

// SpliceEvent represents a splice event
type SpliceEvent struct {
	SyscallEvent

	File          FileEvent `field:"file"`            // File modified by the splice syscall
	PipeEntryFlag uint32    `field:"pipe_entry_flag"` // Entry flag of the "fd_out" pipe passed to the splice syscall
	PipeExitFlag  uint32    `field:"pipe_exit_flag"`  // Exit flag of the "fd_out" pipe passed to the splice syscall
}
