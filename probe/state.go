package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hstern/go-activesync/eas"
)

const storeVersion = 1

// Store keeps the ActiveSync state of any number of devices in memory and
// writes it to one JSON file: on Flush, periodically via AutoSave, and on
// Close. Loading the file at startup lets a restarted exporter or load
// test reuse its registered devices instead of registering them again.
//
// The state per device is tiny (policy key, sync keys, folder list), so
// no database is needed.
type Store struct {
	path string

	mu      sync.Mutex
	devices map[string]*Device
	dirty   atomic.Bool

	saveMu sync.Mutex // serializes file writes

	recovered string // set when a damaged state file was set aside at startup
}

// Recovered returns a message when OpenStore found a damaged or unusable state
// file, moved it aside and started with an empty state; "" otherwise. Callers
// should log it: the devices register again, which is visible on the server.
func (s *Store) Recovered() string {
	return s.recovered
}

type storeFile struct {
	Version int                   `json:"version"`
	Saved   time.Time             `json:"saved"`
	Devices map[string]deviceData `json:"devices"`
}

type deviceData struct {
	PolicyKey string               `json:"policy_key,omitempty"`
	SyncKeys  map[string]string    `json:"sync_keys,omitempty"`
	Folders   map[string]FolderRec `json:"folders,omitempty"`
}

// FolderRec is one remembered folder of the hierarchy.
type FolderRec struct {
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
	Type     int    `json:"type"`
}

// OpenStore loads the state file at path. A missing file is not an error.
// An empty path gives a store that lives in memory only.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, devices: map[string]*Device{}}
	if path == "" {
		return s, nil
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}

	var f storeFile
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != storeVersion {
		// The state is only a cache of what the server told us: a damaged
		// or foreign file must not stop a monitor. Keep it for inspection
		// and start empty (the devices register again).
		reason := fmt.Sprintf("unsupported version %d", f.Version)
		if err != nil {
			reason = err.Error()
		}
		aside := fmt.Sprintf("%s.corrupt-%s", path, time.Now().UTC().Format("20060102T150405Z"))
		if rerr := os.Rename(path, aside); rerr != nil {
			return nil, fmt.Errorf("state %s is unusable (%s) and cannot be moved aside: %w", path, reason, rerr)
		}
		s.recovered = fmt.Sprintf("state file %s was unusable (%s); moved to %s, starting empty", path, reason, aside)
		return s, nil
	}
	for id, d := range f.Devices {
		dev := s.newDevice(id)
		dev.data = d
		dev.normalize()
		s.devices[id] = dev
	}
	return s, nil
}

// Path returns the backing file, or "" for a memory-only store.
func (s *Store) Path() string {
	return s.path
}

// Len returns the number of known devices.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.devices)
}

// Device returns the state of the given device, creating it if needed.
func (s *Store) Device(id string) *Device {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.devices[id]
	if !ok {
		d = s.newDevice(id)
		d.normalize()
		s.devices[id] = d
		s.dirty.Store(true)
	}
	return d
}

func (s *Store) newDevice(id string) *Device {
	return &Device{id: id, store: s}
}

// Flush writes the state file if anything changed since the last write.
func (s *Store) Flush() error {
	if s.path == "" || !s.dirty.Load() {
		return nil
	}
	return s.save()
}

func (s *Store) save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	// Clear the flag before snapshotting: a change that races with the
	// snapshot marks the store dirty again and is picked up next time.
	s.dirty.Store(false)

	out := storeFile{Version: storeVersion, Saved: time.Now().UTC(), Devices: map[string]deviceData{}}

	s.mu.Lock()
	devs := make([]*Device, 0, len(s.devices))
	for _, d := range s.devices {
		devs = append(devs, d)
	}
	s.mu.Unlock()

	for _, d := range devs {
		out.Devices[d.id] = d.snapshot()
	}

	raw, err := json.MarshalIndent(&out, "", " ")
	if err != nil {
		s.dirty.Store(true)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		s.dirty.Store(true)
		return err
	}

	// Temp file + rename: a crash never leaves a truncated state file.
	// Write a temp file, flush it to disk, then replace the state file in one
	// step: a crash leaves either the old or the new state, never a mix. A stale
	// temp file from an interrupted write is simply overwritten.
	tmp := s.path + ".tmp"
	if err := writeFileSync(tmp, raw); err != nil {
		s.dirty.Store(true)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		s.dirty.Store(true)
		return err
	}
	return nil
}

// AutoSave flushes the store every interval until the returned stop
// function is called. stop performs a final flush and returns its error.
// Errors of the periodic flushes are passed to onError (may be nil).
func (s *Store) AutoSave(interval time.Duration, onError func(error)) (stop func() error) {
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := s.Flush(); err != nil && onError != nil {
					onError(err)
				}
			case <-done:
				return
			}
		}
	}()

	var once sync.Once
	return func() error {
		once.Do(func() {
			close(done)
			<-finished
		})
		return s.Flush()
	}
}

// Device is the state of one simulated or monitored ActiveSync device. It
// implements eas.StateStore, so it can be handed to the EAS client.
type Device struct {
	id    string
	store *Store

	run sync.Mutex // held for the duration of a probe of this device

	mu   sync.Mutex
	data deviceData
}

// ID returns the device id.
func (d *Device) ID() string {
	return d.id
}

// TryLock claims the device for one probe. Two probes of the same device
// at the same time would corrupt each other's sync keys, so the second one
// has to be refused (or wait) instead. Unlock releases the claim.
func (d *Device) TryLock() bool {
	return d.run.TryLock()
}

// Unlock releases the claim taken by TryLock.
func (d *Device) Unlock() {
	d.run.Unlock()
}

func (d *Device) normalize() {
	if d.data.SyncKeys == nil {
		d.data.SyncKeys = map[string]string{}
	}
	if d.data.Folders == nil {
		d.data.Folders = map[string]FolderRec{}
	}
}

func (d *Device) snapshot() deviceData {
	d.mu.Lock()
	defer d.mu.Unlock()

	c := deviceData{PolicyKey: d.data.PolicyKey, SyncKeys: map[string]string{}, Folders: map[string]FolderRec{}}
	for k, v := range d.data.SyncKeys {
		c.SyncKeys[k] = v
	}
	for k, v := range d.data.Folders {
		c.Folders[k] = v
	}
	return c
}

func (d *Device) touch() {
	d.store.dirty.Store(true)
}

// Reset forgets the policy key, all sync keys and the folder list.
func (d *Device) Reset() {
	d.mu.Lock()
	d.data = deviceData{}
	d.normalize()
	d.mu.Unlock()
	d.touch()
}

// ResetHierarchy forgets the folder list and every sync key (the folder
// hierarchy key and the keys of all folders) but keeps the policy key. It is
// the recovery the protocol asks for when the server says our sync keys are
// invalid (for example after the server's database was recreated): start again
// at sync key 0.
func (d *Device) ResetHierarchy() {
	d.mu.Lock()
	d.data.Folders = map[string]FolderRec{}
	d.data.SyncKeys = map[string]string{}
	d.mu.Unlock()
	d.touch()
}

// HasPolicyKey reports whether the device was provisioned before.
func (d *Device) HasPolicyKey() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.data.PolicyKey != ""
}

func (d *Device) PolicyKey(_ context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.data.PolicyKey, nil
}

func (d *Device) SetPolicyKey(_ context.Context, key string) error {
	d.mu.Lock()
	d.data.PolicyKey = key
	d.mu.Unlock()
	d.touch()
	return nil
}

func (d *Device) SyncKey(_ context.Context, folderID string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if k, ok := d.data.SyncKeys[folderID]; ok {
		return k, nil
	}
	return "0", nil
}

func (d *Device) SetSyncKey(_ context.Context, folderID, key string) error {
	d.mu.Lock()
	d.data.SyncKeys[folderID] = key
	d.mu.Unlock()
	d.touch()
	return nil
}

// MergeFolders applies a FolderSync delta to the stored folder list. EAS
// only sends the full hierarchy on the first FolderSync; later calls are
// deltas, so the probe keeps the list itself to know where the Inbox is.
func (d *Device) MergeFolders(res *eas.FolderSyncResult) {
	d.mu.Lock()
	for _, f := range res.Added {
		d.data.Folders[f.ServerID] = FolderRec{ParentID: f.ParentID, Name: f.DisplayName, Type: int(f.Type)}
	}
	for _, f := range res.Updated {
		d.data.Folders[f.ServerID] = FolderRec{ParentID: f.ParentID, Name: f.DisplayName, Type: int(f.Type)}
	}
	for _, id := range res.Deleted {
		delete(d.data.Folders, id)
	}
	d.mu.Unlock()
	d.touch()
}

// FolderIDByType returns the server id of the first folder of this type, or "" if there is none.
func (d *Device) FolderIDByType(t eas.FolderType) string {
	d.mu.Lock()
	defer d.mu.Unlock()

	for id, f := range d.data.Folders {
		if eas.FolderType(f.Type) == t {
			return id
		}
	}
	return ""
}

// InboxID returns the server id of the Inbox, or "" if unknown.
func (d *Device) InboxID() string {
	d.mu.Lock()
	defer d.mu.Unlock()

	for id, f := range d.data.Folders {
		if eas.FolderType(f.Type) == eas.FolderTypeInbox {
			return id
		}
	}
	return ""
}

// FolderCount returns the number of known folders.
func (d *Device) FolderCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return len(d.data.Folders)
}

// TypeSummary returns e.g. "Calendar=1 Inbox=1" for diagnostics.
func (d *Device) TypeSummary() string {
	d.mu.Lock()
	defer d.mu.Unlock()

	counts := map[string]int{}
	for _, f := range d.data.Folders {
		counts[eas.FolderType(f.Type).String()]++
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", n, counts[n]))
	}
	return strings.Join(parts, " ")
}

var _ eas.StateStore = (*Device)(nil)

// writeFileSync writes the file and forces it to disk before returning.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
