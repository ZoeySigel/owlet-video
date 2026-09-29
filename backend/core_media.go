package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Copy all media and upload fragments to an isolated directory. Existing files
// are accepted only when their content matches; nothing is overwritten/deleted.
func copyCoreMedia(sourceRoot, targetRoot string) error {
	if sourceRoot == "" || targetRoot == "" {
		return errors.New("SOURCE_DATA_DIR and TARGET_DATA_DIR are required for apply")
	}
	source, err := filepath.Abs(sourceRoot)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(targetRoot)
	if err != nil {
		return err
	}
	for _, pair := range [][2]string{{source, target}, {target, source}} {
		rel, e := filepath.Rel(pair[0], pair[1])
		if e != nil {
			return e
		}
		if rel == "." || (!filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return errors.New("source and target data directories must be disjoint")
		}
	}
	// Refuse symlink roots/ancestors, including Windows junctions resolved to a
	// different location. The target root is created only after these checks.
	for _, root := range []string{source, target} {
		for p := root; ; p = filepath.Dir(p) {
			info, e := os.Lstat(p)
			if e == nil && info.Mode()&os.ModeSymlink != 0 {
				return errors.New("migration directory cannot contain symlink ancestors")
			}
			if e != nil && !os.IsNotExist(e) {
				return e
			}
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	manifest := map[string]string{}
	err = filepath.WalkDir(source, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink %s", path)
		}
		if d.IsDir() {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file %s", path)
		}
		rel, e := filepath.Rel(source, path)
		if e != nil {
			return e
		}
		digest, e := coreFileDigest(path)
		if e != nil {
			return e
		}
		manifest[rel] = digest
		return nil
	})
	if err != nil {
		return err
	}
	if _, err := os.Stat(target); err == nil {
		if err := filepath.WalkDir(target, func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("target contains symlink")
			}
			if d.IsDir() {
				return nil
			}
			rel, e := filepath.Rel(target, path)
			if e != nil {
				return e
			}
			want, ok := manifest[rel]
			if !ok {
				return fmt.Errorf("target contains unrelated file %s", rel)
			}
			got, e := coreFileDigest(path)
			if e != nil {
				return e
			}
			if got != want {
				return fmt.Errorf("target file differs: %s", rel)
			}
			return nil
		}); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for rel, want := range manifest {
		dest := filepath.Join(target, rel)
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(dest), 0750); err != nil {
				return err
			}
			input, err := os.Open(filepath.Join(source, rel))
			if err != nil {
				return err
			}
			output, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
			if err != nil {
				input.Close()
				return err
			}
			_, copyErr := io.Copy(output, input)
			syncErr := output.Sync()
			closeErr := output.Close()
			input.Close()
			if copyErr != nil {
				return copyErr
			}
			if syncErr != nil {
				return syncErr
			}
			if closeErr != nil {
				return closeErr
			}
		} else if err != nil {
			return err
		}
		got, err := coreFileDigest(dest)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("copied file checksum mismatch: %s", rel)
		}
	}
	// Detect source modifications during copying before allowing DB migration.
	for rel, want := range manifest {
		got, err := coreFileDigest(filepath.Join(source, rel))
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("source file changed while copying: %s", rel)
		}
	}
	return nil
}

func coreFileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
