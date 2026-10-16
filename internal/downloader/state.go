package downloader

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// chunkState tracks resume progress for a single byte range of a file.
// End is inclusive; End == -1 means "until EOF" (used when the chunk's
// length isn't known ahead of time, i.e. the transfer isn't resumable).
type chunkState struct {
	Start      int64 `json:"start"`
	End        int64 `json:"end"`
	Downloaded int64 `json:"downloaded"`
}

// state is the on-disk resume record for one in-progress download. It is
// safe for concurrent use: each chunk goroutine mutates only its own
// chunkState entry, but the periodic state save and progress reporter read
// across all of them, so access is serialized through mu.
type state struct {
	mu     sync.Mutex
	saveMu sync.Mutex

	URL       string       `json:"url"`
	Size      int64        `json:"size"`
	Resumable bool         `json:"resumable"`
	NumChunks int          `json:"num_chunks"`
	Chunks    []chunkState `json:"chunks"`

	path        string
	lastSaveUCn int64 // unix nano of last successful save, for throttling
}

func statePath(dest string) string { return dest + ".part.json" }
func tempPath(dest string) string  { return dest + ".part" }

// addProgress records n additional bytes downloaded for chunk idx.
func (s *state) addProgress(idx int, n int64) {
	s.mu.Lock()
	s.Chunks[idx].Downloaded += n
	s.mu.Unlock()
}

// chunkDownloaded returns bytes already downloaded for chunk idx.
func (s *state) chunkDownloaded(idx int) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Chunks[idx].Downloaded
}

// resetChunk zeroes progress for chunk idx, used before a from-scratch
// retry of a non-resumable (no Range support) transfer.
func (s *state) resetChunk(idx int) {
	s.mu.Lock()
	s.Chunks[idx].Downloaded = 0
	s.mu.Unlock()
}

// totalDownloaded sums progress across all chunks.
func (s *state) totalDownloaded() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sum int64
	for _, c := range s.Chunks {
		sum += c.Downloaded
	}
	return sum
}

func (s *state) marshal() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.MarshalIndent(struct {
		URL       string       `json:"url"`
		Size      int64        `json:"size"`
		Resumable bool         `json:"resumable"`
		NumChunks int          `json:"num_chunks"`
		Chunks    []chunkState `json:"chunks"`
	}{s.URL, s.Size, s.Resumable, s.NumChunks, s.Chunks}, "", "  ")
}

// save persists the current state unconditionally. Multiple chunk
// goroutines may call save/saveThrottled concurrently; saveMu serializes
// the write+rename sequence so they don't race on the same temp file
// (fatal on Windows, where a rename target/source held open by another
// in-flight writer fails outright).
func (s *state) save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	data, err := s.marshal()
	if err != nil {
		return fmt.Errorf("marshal resume state: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write resume state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("commit resume state: %w", err)
	}
	atomic.StoreInt64(&s.lastSaveUCn, time.Now().UnixNano())
	return nil
}

// saveThrottled persists state at most once per throttleInterval, to
// avoid a disk write on every 32KB read.
const throttleInterval = 2 * time.Second

func (s *state) saveThrottled() error {
	last := atomic.LoadInt64(&s.lastSaveUCn)
	if time.Since(time.Unix(0, last)) < throttleInterval {
		return nil
	}
	return s.save()
}

// buildChunks splits [0, size) into n contiguous inclusive ranges.
func buildChunks(size int64, n int) []chunkState {
	if n < 1 {
		n = 1
	}
	chunks := make([]chunkState, n)
	base := size / int64(n)
	remainder := size % int64(n)
	var offset int64
	for i := 0; i < n; i++ {
		length := base
		if int64(i) < remainder {
			length++
		}
		chunks[i] = chunkState{Start: offset, End: offset + length - 1}
		offset += length
	}
	if size <= 0 {
		chunks = []chunkState{{Start: 0, End: -1}}
	}
	return chunks
}

// loadOrCreateState loads a persisted resume record for dest if the
// current transfer can resume it, or creates a fresh one otherwise. Only
// a resumable transfer resumes, and only from a state that was itself
// resumable, has the same URL and size, and whose chunks describe the
// file exactly (see validChunks): a server without Range support sends
// the file from byte 0 again, so its saved progress is worthless. A
// missing, corrupt or unusable state file means a fresh start, not an
// error.
func loadOrCreateState(dest, url string, size int64, resumable bool, numChunks int) (st *state, resumedFromDisk bool) {
	sp := statePath(dest)
	if !resumable {
		return newState(url, size, resumable, numChunks, sp), false
	}
	if data, readErr := os.ReadFile(sp); readErr == nil {
		var loaded state
		if jsonErr := json.Unmarshal(data, &loaded); jsonErr == nil {
			if loaded.Resumable && loaded.URL == url && loaded.Size == size && validChunks(loaded.Chunks, size) {
				loaded.path = sp
				return &loaded, true
			}
		}
	}

	return newState(url, size, resumable, numChunks, sp), false
}

// validChunks reports whether chunks are contiguous inclusive ranges
// covering exactly [0, size), each with progress within its own length.
func validChunks(chunks []chunkState, size int64) bool {
	if len(chunks) == 0 || size <= 0 {
		return false
	}
	next := int64(0)
	for _, c := range chunks {
		if c.Start != next || c.End < c.Start || c.Downloaded < 0 || c.Downloaded > c.End-c.Start+1 {
			return false
		}
		next = c.End + 1
	}
	return next == size
}

// newState builds a fresh resume record with no progress.
func newState(url string, size int64, resumable bool, numChunks int, path string) *state {
	chunks := buildChunks(size, numChunks)
	return &state{
		URL:       url,
		Size:      size,
		Resumable: resumable,
		NumChunks: len(chunks),
		Chunks:    chunks,
		path:      path,
	}
}
