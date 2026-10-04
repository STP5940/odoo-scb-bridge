package sftp

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"odoo-scb-bridge/internal/models"

	pkgsftp "github.com/pkg/sftp"
)

// userFilesystem exposes one account's home as its SFTP root. os.Root keeps
// every operation inside that directory, including paths containing symlinks.
type userFilesystem struct {
	root  *os.Root
	perms models.User
	mu    sync.Mutex
	files map[string]struct{}
}

func newUserFilesystem(root *os.Root, user models.User) *userFilesystem {
	return &userFilesystem{root: root, perms: user, files: make(map[string]struct{})}
}

func (fs *userFilesystem) markModified(name string) {
	fs.mu.Lock()
	fs.files[name] = struct{}{}
	fs.mu.Unlock()
}

func (fs *userFilesystem) modifiedFiles() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	files := make([]string, 0, len(fs.files))
	for name := range fs.files {
		files = append(files, name)
	}
	return files
}

func (fs *userFilesystem) handlers() pkgsftp.Handlers {
	return pkgsftp.Handlers{FileGet: fs, FilePut: fs, FileCmd: fs, FileList: fs}
}

func (fs *userFilesystem) relativePath(raw string) (string, error) {
	raw = strings.ReplaceAll(raw, `\`, "/")
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", errors.New("path outside user home is not allowed")
		}
	}
	cleaned := path.Clean("/" + raw)
	rel := strings.TrimPrefix(cleaned, "/")
	if rel == "" {
		rel = "."
	}
	return filepath.FromSlash(rel), nil
}

func (fs *userFilesystem) Fileread(req *pkgsftp.Request) (io.ReaderAt, error) {
	if !fs.perms.CanRead {
		return nil, errors.New("download permission denied")
	}
	name, err := fs.relativePath(req.Filepath)
	if err != nil {
		return nil, err
	}
	return fs.root.Open(name)
}

func (fs *userFilesystem) Filewrite(req *pkgsftp.Request) (io.WriterAt, error) {
	if !fs.perms.CanWrite {
		return nil, errors.New("upload permission denied")
	}
	name, err := fs.relativePath(req.Filepath)
	if err != nil {
		return nil, err
	}
	flags := req.Pflags()
	openFlags := os.O_WRONLY
	if flags.Read {
		openFlags = os.O_RDWR
	}
	if flags.Creat {
		openFlags |= os.O_CREATE
	}
	if flags.Trunc {
		openFlags |= os.O_TRUNC
	}
	if flags.Excl {
		openFlags |= os.O_EXCL
	}
	file, err := fs.root.OpenFile(name, openFlags, 0660)
	if err == nil {
		fs.markModified(filepath.ToSlash(name))
	}
	return file, err
}

func (fs *userFilesystem) Filelist(req *pkgsftp.Request) (pkgsftp.ListerAt, error) {
	name, err := fs.relativePath(req.Filepath)
	if err != nil {
		return nil, err
	}
	switch req.Method {
	case "List":
		if !fs.perms.CanList {
			return nil, errors.New("directory listing permission denied")
		}
		directory, err := fs.root.Open(name)
		if err != nil {
			return nil, err
		}
		entries, err := directory.ReadDir(-1)
		_ = directory.Close()
		if err != nil {
			return nil, err
		}
		items := make([]os.FileInfo, 0, len(entries))
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			items = append(items, info)
		}
		return infoLister(items), nil
	case "Stat":
		if !fs.perms.CanList && !fs.perms.CanRead && !fs.perms.CanWrite {
			return nil, errors.New("file information permission denied")
		}
		info, err := fs.root.Stat(name)
		if err != nil {
			return nil, err
		}
		return infoLister([]os.FileInfo{info}), nil
	default:
		return nil, errors.New("operation not supported")
	}
}

func (fs *userFilesystem) RealPath(raw string) (string, error) {
	name, err := fs.relativePath(raw)
	if err != nil {
		return "", err
	}
	return "/" + filepath.ToSlash(name), nil
}

func (fs *userFilesystem) Filecmd(req *pkgsftp.Request) error {
	name, err := fs.relativePath(req.Filepath)
	if err != nil {
		return err
	}
	switch req.Method {
	case "Mkdir":
		if !fs.perms.CanMkdir {
			return errors.New("create directory permission denied")
		}
		return fs.root.Mkdir(name, 0770)
	case "Rename":
		if !fs.perms.CanRename {
			return errors.New("rename permission denied")
		}
		target, err := fs.relativePath(req.Target)
		if err != nil {
			return err
		}
		if err := fs.root.Rename(name, target); err != nil {
			return err
		}
		fs.markModified(filepath.ToSlash(target))
		return nil
	case "Remove", "Rmdir":
		if !fs.perms.CanDelete {
			return errors.New("delete permission denied")
		}
		return fs.root.Remove(name)
	case "Setstat":
		if !fs.perms.CanWrite {
			return errors.New("modify permission denied")
		}
		attrs, flags := req.Attributes(), req.AttrFlags()
		if flags.Size {
			file, err := fs.root.OpenFile(name, os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			err = file.Truncate(int64(attrs.Size))
			closeErr := file.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		if flags.Permissions {
			if err := fs.root.Chmod(name, attrs.FileMode()); err != nil {
				return err
			}
		}
		if flags.Acmodtime {
			if err := fs.root.Chtimes(name, time.Unix(int64(attrs.Atime), 0), time.Unix(int64(attrs.Mtime), 0)); err != nil {
				return err
			}
		}
		if flags.Size || flags.Permissions || flags.Acmodtime {
			fs.markModified(filepath.ToSlash(name))
		}
		return nil
	default:
		return errors.New("operation not supported")
	}
}

type infoLister []os.FileInfo

func (items infoLister) ListAt(dst []os.FileInfo, offset int64) (int, error) {
	if offset < 0 || offset >= int64(len(items)) {
		return 0, io.EOF
	}
	n := copy(dst, items[offset:])
	if int(offset)+n >= len(items) {
		return n, io.EOF
	}
	return n, nil
}
