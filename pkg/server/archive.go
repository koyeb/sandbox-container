package server

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Archive routes: a directory's tree as a gzip'd tar, in both directions.
// Everything streams through io.Copy's pooled buffers, so memory stays flat
// no matter the archive size. Hardlinked paths are archived as independent
// copies: the extractor's hardlink support covers foreign archives.

// ArchiveResult reports what a PUT /files/archive extracted.
type ArchiveResult struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// parseArchiveRequest validates a GET /files/archive request — path present,
// exclude patterns well-formed, root an existing directory — writing the
// error response itself, and returns the resolved root to walk with the
// exclude patterns.
func parseArchiveRequest(w http.ResponseWriter, r *http.Request) (
	walkRoot string, excludes []string, ok bool) {
	root, hasPath := pathParam(w, r)
	if !hasPath {
		return "", nil, false
	}
	excludes = r.URL.Query()["exclude"]
	for _, pat := range excludes {
		if _, err := path.Match(pat, ""); err != nil {
			writeFileError(w, http.StatusBadRequest, fmt.Sprintf("bad exclude pattern %q", pat))
			return "", nil, false
		}
	}
	info, err := os.Stat(root)
	if err != nil {
		writeFSError(w, err)
		return "", nil, false
	}
	if !info.IsDir() {
		writeFileError(w, http.StatusBadRequest, fmt.Sprintf("%s is not a directory", root))
		return "", nil, false
	}
	// Walk the directory a symlink points to, as /files/list does.
	walkRoot, err = filepath.EvalSymlinks(root)
	if err != nil {
		writeFSError(w, err)
		return "", nil, false
	}
	return walkRoot, excludes, true
}

// putArchiveHandler extracts a gzip'd tar into the directory at path,
// creating it if needed. Entries already extracted stay in place when the
// stream ends short, as tar itself leaves them.
func (s *Server) putArchiveHandler(w http.ResponseWriter, r *http.Request) {
	root, ok := pathParam(w, r)
	if !ok {
		return
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		slog.Debug("Failed to create archive root", "path", root, "error", err)
		writeFSError(w, err)
		return
	}
	gz, err := gzip.NewReader(r.Body)
	if err != nil {
		writeFileError(w, http.StatusBadRequest, fmt.Sprintf("reading gzip stream: %v", err))
		return
	}
	defer gz.Close()

	res, dirs, err := extractTar(tar.NewReader(gz), root)
	if err != nil {
		slog.Debug("Failed to extract archive", "path", root, "error", err)
		writeExtractError(w, err)
		return
	}
	// Directory modes and mtimes come last: writing entries into a directory
	// bumps its mtime, and a restrictive mode applied any earlier would lock
	// the directory's own contents out of extraction.
	if err := restoreDirs(dirs); err != nil {
		writeExtractError(w, err)
		return
	}
	slog.Debug("Archive extracted", "path", root, "files", res.Files, "bytes", res.Bytes)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// extractTar walks the tar stream, writing each entry under root. dirs
// collects the directories whose mtimes restoreDirTimes applies once the
// whole archive is in place.
func extractTar(tr *tar.Reader, root string) (res ArchiveResult, dirs []dirRestore, err error) {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return res, dirs, nil
		}
		if err != nil {
			return res, dirs, err
		}
		dt, files, bytes, err := extractEntry(tr, root, hdr)
		if err != nil {
			return res, dirs, err
		}
		if dt != nil {
			dirs = append(dirs, *dt)
		}
		res.Files += files
		res.Bytes += bytes
	}
}

// extractEntry writes one member under root and reports what it extracted:
// the dirRestore to restore at the end when it made a directory, and the files
// and bytes it wrote.
func extractEntry(tr *tar.Reader, root string, hdr *tar.Header) (*dirRestore, int, int64, error) {
	name, err := cleanEntryName(hdr.Name)
	if err != nil {
		return nil, 0, 0, err
	}
	dest, err := prepareMemberDest(root, name)
	if err != nil {
		return nil, 0, 0, err
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		dt, err := extractDirEntry(dest, hdr)
		return dt, 0, 0, err
	case tar.TypeReg:
		n, err := extractFile(tr, dest, hdr)
		return nil, 1, n, err
	case tar.TypeSymlink:
		// The link is stored byte-identical to the archive; only its
		// resolved target has to stay inside the root.
		return nil, 0, 0, extractSymlink(dest, name, hdr)
	case tar.TypeLink:
		// A hardlink's Linkname is the member path of the file it links
		// to, relative to the archive root — not to this entry's directory,
		// unlike a symlink's target.
		return nil, 1, 0, extractHardlink(root, dest, hdr)
	case tar.TypeXGlobalHeader:
		// PAX global metadata, not a member.
		return nil, 0, 0, nil
	default:
		return nil, 0, 0, fmt.Errorf("entry %q: unsupported type %q", hdr.Name, string(hdr.Typeflag))
	}
}

// prepareMemberDest returns the member's destination under root, after
// refusing links in the way and creating the parent directories: directory
// members are a courtesy many archives skip, so every entry's parents are
// made as tar does.
func prepareMemberDest(root, name string) (string, error) {
	if err := inTheWay(root, name); err != nil {
		return "", err
	}
	dest := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	return dest, nil
}

// extractDirEntry creates one directory. Its archived mode is not applied
// here: restoreDirs sets it after the contents, so a restrictive mode can't
// lock them out of extraction.
func extractDirEntry(dest string, hdr *tar.Header) (*dirRestore, error) {
	// MkdirAll would follow a link at dest and merge into its target.
	if info, err := os.Lstat(dest); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return nil, &fs.PathError{Op: "extract", Path: dest, Err: fs.ErrExist}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}
	return &dirRestore{path: dest, mode: fs.FileMode(hdr.Mode & 0o7777), mtime: hdr.ModTime}, nil
}

// extractSymlink stores one link byte-identical to the archive, after
// checking its resolved target stays inside the root.
func extractSymlink(dest, name string, hdr *tar.Header) error {
	if err := resolveLinkTarget(name, hdr.Linkname); err != nil {
		return err
	}
	if err := clearMemberPath(dest); err != nil {
		return err
	}
	return os.Symlink(hdr.Linkname, dest)
}

// extractHardlink hardlinks dest to the member the archive names.
func extractHardlink(root, dest string, hdr *tar.Header) error {
	target, err := cleanEntryName(hdr.Linkname)
	if err != nil {
		return err
	}
	// link(2) follows symlinks in the target's components — on BSD even
	// the final one — so the target path gets the same guards as a member's
	// own: never through a link.
	if err := inTheWay(root, target); err != nil {
		return err
	}
	targetPath := filepath.Join(root, filepath.FromSlash(target))
	if info, err := os.Lstat(targetPath); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return &fs.PathError{Op: "link", Path: targetPath, Err: fs.ErrExist}
	}
	if err := clearMemberPath(dest); err != nil {
		return err
	}
	return os.Link(targetPath, dest)
}

// cleanEntryName validates a tar entry name: relative, and staying inside
// the root after cleaning. "." is allowed: tar czf x.tgz -C dir . writes
// "./"-prefixed entries whose cleaned form is the root itself.
func cleanEntryName(name string) (string, error) {
	cleaned := path.Clean(name)
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("entry %q: name must stay inside the archive root", name)
	}
	return cleaned, nil
}

// inTheWay reports a symlink among a member's existing ancestor components
// under root: extraction never writes through a pre-existing link, so a
// merge can't escape the archive root. The member's own path is not
// checked here — clearMemberPath and the dir guard handle what sits at it.
func inTheWay(root, name string) error {
	parts := strings.Split(name, "/")
	p := root
	for _, part := range parts[:len(parts)-1] {
		p = filepath.Join(p, part)
		info, err := os.Lstat(p)
		if err != nil {
			// Nothing deeper can exist; the archive creates the rest.
			return nil
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return &fs.PathError{Op: "extract", Path: p, Err: fs.ErrExist}
		}
	}
	return nil
}

// clearMemberPath removes what sits at a leaf member's destination, as tar
// does: a file or a link in the way is replaced — never written through —
// and a re-extraction over the archive's own output can't fail on the modes
// or links it restored. A directory in the way is left for the create to
// refuse.
func clearMemberPath(dest string) error {
	info, err := os.Lstat(dest)
	if err != nil || info.IsDir() {
		return nil
	}
	return os.Remove(dest)
}

// resolveLinkTarget validates the Linkname of a symlink entry named name. A
// relative target is read relative to the entry's directory, as the filesystem
// would read it; an absolute or escaping target is refused.
func resolveLinkTarget(name, target string) error {
	if target == "" || path.IsAbs(target) {
		return fmt.Errorf(
			"entry %q: link target %q must be relative and inside the archive root", name, target)
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("entry %q: link target %q escapes the archive root", name, target)
	}
	return nil
}

// extractFile streams one member into dest and then applies its mode and
// mtime. The mode comes after the copy: the create mode goes through the
// process's umask, the chmod doesn't.
func extractFile(tr *tar.Reader, dest string, hdr *tar.Header) (int64, error) {
	if err := clearMemberPath(dest); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fs.FileMode(hdr.Mode&0o7777))
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, tr)
	if closeErr := f.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return n, copyErr
	}
	if err := os.Chmod(dest, fs.FileMode(hdr.Mode&0o7777)); err != nil {
		return n, err
	}
	// The tar format can't say "no mtime": an entry written with none reads
	// back as the epoch, and extracts as the epoch, like tar itself.
	return n, os.Chtimes(dest, hdr.ModTime, hdr.ModTime)
}

// dirRestore is a directory whose mode and mtime are restored after
// extraction, once its contents are in place.
type dirRestore struct {
	path  string
	mode  fs.FileMode
	mtime time.Time
}

// restoreDirs applies the archive's directory modes and mtimes, children
// before parents: a restrictive mode lands after the directory's contents,
// and the mtime after whatever bumped it.
func restoreDirs(dirs []dirRestore) error {
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].path > dirs[j].path })
	for _, d := range dirs {
		if err := os.Chmod(d.path, d.mode); err != nil {
			return err
		}
		if err := os.Chtimes(d.path, d.mtime, d.mtime); err != nil {
			return err
		}
	}
	return nil
}

// writeExtractError classifies a failed extraction: an entry or stream
// error is the client's (400), a filesystem error keeps its own status.
func writeExtractError(w http.ResponseWriter, err error) {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) {
		writeFSError(w, err)
	} else {
		writeFileError(w, http.StatusBadRequest, err.Error())
	}
}

func (s *Server) getArchiveHandler(w http.ResponseWriter, r *http.Request) {
	walkRoot, excludes, ok := parseArchiveRequest(w, r)
	if !ok {
		return
	}

	slog.Debug("Archiving directory", "path", walkRoot, "excludes", excludes)
	w.Header().Set("Content-Type", "application/gzip")
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	entries := 0
	err := filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, walkErr error) error {
		emitted, err := writeArchiveEntry(tw, walkRoot, p, d, walkErr, excludes)
		if emitted {
			entries++
		}
		return err
	})
	if err != nil {
		// No Close on failure, either way: before the first entry the body must
		// be the JSON error alone (a deferred Close would append gzip and tar
		// trailer bytes after it), and past it the stream must end cut, without
		// a footer or trailer that would make a truncated archive look
		// complete to the client.
		if entries == 0 {
			slog.Debug("Failed to start archive", "path", walkRoot, "error", err)
			writeFSError(w, err)
		} else {
			slog.Debug("Archive stopped mid-stream", "path", walkRoot, "entries", entries, "error", err)
		}
		return
	}
	if err := tw.Close(); err != nil {
		slog.Debug("Failed to finish archive", "path", walkRoot, "error", err)
	}
	if err := gz.Close(); err != nil {
		slog.Debug("Failed to finish archive", "path", walkRoot, "error", err)
	}
	slog.Debug("Directory archived", "path", walkRoot, "entries", entries)
}

// writeArchiveEntry writes one tar entry for a walk position and returns the
// value the WalkDir callback should return. emitted reports that a header was
// written. The root itself is not an entry; exclude patterns match any suffix
// of the relative path, and a matching directory is skipped whole.
func writeArchiveEntry(tw *tar.Writer, walkRoot, p string, d fs.DirEntry,
	walkErr error, excludes []string) (emitted bool, err error) {
	if walkErr != nil {
		return false, walkErr
	}
	if p == walkRoot {
		return false, nil
	}
	rel, err := filepath.Rel(walkRoot, p)
	if err != nil {
		return false, err
	}
	rel = filepath.ToSlash(rel)
	if excluded(rel, excludes) {
		if d.IsDir() {
			return false, filepath.SkipDir
		}
		return false, nil
	}
	fi, err := d.Info()
	if err != nil {
		return false, err
	}
	// Special files (FIFOs, sockets, devices) are skipped: they can't be
	// recreated safely on extraction.
	if !archivable(fi.Mode()) {
		slog.Debug("Skipping unarchivable file", "path", p, "type", fileType(fi.Mode()))
		return false, nil
	}
	return writeTarMember(tw, p, rel, fi)
}

// writeTarMember writes one tree member's header, and its content when it
// is a regular file. Symlinks are archived as links, not followed. A
// regular file is opened before its header is written, so an unreadable
// one fails the request before any of its bytes reach the stream.
func writeTarMember(tw *tar.Writer, p, rel string, fi fs.FileInfo) (emitted bool, err error) {
	var f *os.File
	if fi.Mode().IsRegular() {
		if f, err = openRegularChecked(p); err != nil {
			return false, err
		}
		defer f.Close()
	}
	hdr, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return false, err
	}
	hdr.Name = rel
	if hdr.Typeflag == tar.TypeSymlink {
		if hdr.Linkname, err = os.Readlink(p); err != nil {
			return false, err
		}
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return false, err
	}
	if f == nil {
		return true, nil
	}
	if _, err := io.Copy(tw, f); err != nil {
		// The header is already on the stream: the caller must treat this as
		// a mid-stream cut, not as an error it can still answer with a status.
		return true, err
	}
	return true, nil
}

// openRegularChecked opens p, refusing anything that is not a regular
// file at the moment of the open: the walk's Info can go stale mid-walk, and
// a file swapped for a writer-less FIFO would block the copy forever.
func openRegularChecked(p string) (*os.File, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, notRegularError{path: p, kind: fileType(info.Mode())}
	}
	return f, nil
}

// excluded reports whether any pattern matches rel or one of its suffixes
// starting at a path boundary, as tar --exclude does: "node_modules" drops
// sub/node_modules too, "*.log" drops .log files at any depth.
func excluded(rel string, patterns []string) bool {
	for _, pat := range patterns {
		for name := rel; ; {
			if ok, _ := path.Match(pat, name); ok {
				return true
			}
			i := strings.IndexByte(name, '/')
			if i < 0 {
				break
			}
			name = name[i+1:]
		}
	}
	return false
}

// archivable reports whether a file becomes a tar entry: regular files,
// directories and symlinks.
func archivable(mode fs.FileMode) bool {
	return mode.IsRegular() || mode.IsDir() || mode&fs.ModeSymlink != 0
}
