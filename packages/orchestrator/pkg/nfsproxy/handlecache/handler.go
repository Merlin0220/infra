// Package handlecache maps NFS file handles to a path on a mounted volume.
//
// NFSv3 handles must outlive any open file, but the proxy can only remember
// a bounded number, and a forgotten handle is stale to the guest. Each
// successful mount gets its own LRU so one mount touching many paths cannot
// evict the handles another mount holds open, and all of a volume's mount
// caches are dropped as soon as its sandbox goes away rather than aging out.
//
// Only registered volumes keep handles. A request that finishes after its
// sandbox was released still asks for handles on the released filesystem; it
// gets handles that are stale on their next use, and nothing is retained.
package handlecache

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"strings"
	"sync"

	"github.com/go-git/go-billy/v5"
	"github.com/hashicorp/golang-lru/v2/simplelru"
	"github.com/willscott/go-nfs"
)

// A handle is the mount's ID followed by the entry's ID. Both are random:
// the proxy does not check which sandbox presents a handle, so a handle must
// not be guessable.
const (
	idSize     = 8
	handleSize = 2 * idSize
)

type Handler struct {
	nfs.Handler

	limit int

	mu        sync.RWMutex
	mounts    map[uint64]*mount
	byMounted map[billy.Filesystem]map[uint64]struct{}
}

type mount struct {
	id uint64
	fs billy.Filesystem

	mu      sync.Mutex
	handles *simplelru.LRU[uint64, []string]
	byPath  map[string]uint64
}

type mountedFS struct {
	billy.Filesystem
	mountID uint64
}

func (f *mountedFS) Unwrap() billy.Filesystem { return f.Filesystem }
func (f *mountedFS) cacheMountID() uint64     { return f.mountID }

var _ nfs.Handler = (*Handler)(nil)
var _ billy.Filesystem = (*mountedFS)(nil)

// Wrap remembers up to limit handles for each successful mount.
func Wrap(h nfs.Handler, limit int) *Handler {
	return &Handler{
		Handler:   h,
		limit:     limit,
		mounts:    make(map[uint64]*mount),
		byMounted: make(map[billy.Filesystem]map[uint64]struct{}),
	}
}

func (h *Handler) Mount(ctx context.Context, conn net.Conn, request nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	status, filesystem, auth := h.Handler.Mount(ctx, conn, request)
	if status != nfs.MountStatusOk || filesystem == nil {
		return status, filesystem, auth
	}

	return status, h.attach(filesystem), auth
}

func (h *Handler) ToHandle(_ context.Context, fs billy.Filesystem, path []string) []byte {
	m := h.mountFor(fs)

	m.mu.Lock()
	defer m.mu.Unlock()

	key := pathKey(path)
	id, ok := m.byPath[key]
	if ok {
		// Get marks the entry as recently used, so a path the guest keeps
		// asking for is not the next one evicted.
		m.handles.Get(id)
	} else {
		id = randomID()
		for m.handles.Contains(id) {
			id = randomID()
		}
		m.handles.Add(id, append([]string(nil), path...))
		m.byPath[key] = id
	}

	return format(m.id, id)
}

func (h *Handler) FromHandle(_ context.Context, handle []byte) (billy.Filesystem, []string, error) {
	m, id, ok := h.parse(handle)
	if !ok {
		return nil, nil, &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusStale}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	path, ok := m.handles.Get(id)
	if !ok {
		return nil, nil, &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusStale}
	}

	// Keep the parent directories at least as fresh as the path, so a
	// directory never ages out while a file in it is still in use.
	for i := range path {
		if parent, ok := m.byPath[pathKey(path[:i])]; ok {
			m.handles.Get(parent)
		}
	}
	m.handles.Get(id)

	return m.fs, append([]string(nil), path...), nil
}

func (h *Handler) InvalidateHandle(_ context.Context, _ billy.Filesystem, handle []byte) error {
	m, id, ok := h.parse(handle)
	if !ok {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.handles.Remove(id)

	return nil
}

// HandleLimit is per mount. go-nfs uses half of it to cap how many entries
// one directory listing page returns.
func (h *Handler) HandleLimit() int {
	return h.limit
}

// Release forgets every handle for every mount of the volume mounted as fs.
// Later requests with those handles are stale.
func (h *Handler) Release(mounted billy.Filesystem) {
	mounted = unwrap(mounted)

	h.mu.Lock()
	defer h.mu.Unlock()

	for mountID := range h.byMounted[mounted] {
		delete(h.mounts, mountID)
	}
	delete(h.byMounted, mounted)
}

// Register marks the volume mounted as fs as active. Each subsequent Mount of
// it gets a cache that Release removes.
func (h *Handler) Register(mounted billy.Filesystem) {
	mounted = unwrap(mounted)

	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.byMounted[mounted]; ok {
		return
	}

	h.byMounted[mounted] = make(map[uint64]struct{})
}

// attach gives one successful NFS mount its own cache. If its backing volume
// was released while Mount was in flight, the returned filesystem has no
// registered cache and every handle minted from it is stale.
func (h *Handler) attach(fs billy.Filesystem) billy.Filesystem {
	base := unwrap(fs)
	m := h.newMount()

	h.mu.Lock()
	for {
		m.id = randomID()
		if _, taken := h.mounts[m.id]; !taken {
			break
		}
	}
	mounted := &mountedFS{Filesystem: fs, mountID: m.id}
	m.fs = mounted
	if mountIDs, ok := h.byMounted[base]; ok {
		h.mounts[m.id] = m
		mountIDs[m.id] = struct{}{}
	}
	h.mu.Unlock()

	return mounted
}

// mountFor finds the cache belonging to fs. A filesystem without a registered
// mount gets a cache nobody can look up, so its handles are stale.
func (h *Handler) mountFor(fs billy.Filesystem) *mount {
	if mountID, ok := findMountID(fs); ok {
		h.mu.RLock()
		m, found := h.mounts[mountID]
		h.mu.RUnlock()
		if found {
			return m
		}
	}

	m := h.newMount()
	m.id = randomID()

	return m
}

func findMountID(fs billy.Filesystem) (uint64, bool) {
	for fs != nil {
		if mounted, ok := fs.(interface{ cacheMountID() uint64 }); ok {
			return mounted.cacheMountID(), true
		}

		wrapper, ok := fs.(interface{ Unwrap() billy.Filesystem })
		if !ok {
			return 0, false
		}
		fs = wrapper.Unwrap()
	}

	return 0, false
}

func (h *Handler) newMount() *mount {
	m := &mount{byPath: make(map[string]uint64)}
	m.handles, _ = simplelru.NewLRU(h.limit, func(id uint64, path []string) {
		if key := pathKey(path); m.byPath[key] == id {
			delete(m.byPath, key)
		}
	})

	return m
}

// format builds the handle parse reads: the mount's ID, then the entry's.
func format(mountID, entryID uint64) []byte {
	handle := make([]byte, 0, handleSize)
	handle = binary.BigEndian.AppendUint64(handle, mountID)

	return binary.BigEndian.AppendUint64(handle, entryID)
}

func (h *Handler) parse(handle []byte) (*mount, uint64, bool) {
	if len(handle) != handleSize {
		return nil, 0, false
	}

	h.mu.RLock()
	m, ok := h.mounts[binary.BigEndian.Uint64(handle[:idSize])]
	h.mu.RUnlock()

	return m, binary.BigEndian.Uint64(handle[idSize:]), ok
}

// unwrap finds the filesystem the volume was mounted as, under the
// middleware that wraps it.
func unwrap(fs billy.Filesystem) billy.Filesystem {
	for {
		wrapper, ok := fs.(interface{ Unwrap() billy.Filesystem })
		if !ok {
			return fs
		}
		fs = wrapper.Unwrap()
	}
}

func pathKey(path []string) string {
	return strings.Join(path, "/")
}

func randomID() uint64 {
	var b [idSize]byte
	_, _ = rand.Read(b[:])

	return binary.BigEndian.Uint64(b[:])
}
