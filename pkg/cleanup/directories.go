/*
Copyright 2021 k0s authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cleanup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"k8s.io/mount-utils"
)

type directories struct {
	Config *Config
}

// Name returns the name of the step
func (d *directories) Name() string {
	return "remove directories step"
}

// Run removes all kubelet mounts and deletes generated dataDir and runDir
func (d *directories) Run() error {
	// unmount any leftover overlays (such as in alpine)
	mounter := mount.New("")
	procMounts, err := mounter.List()
	if err != nil {
		return err
	}

	var dataDirMounted bool

	// ensure that we don't delete any persistent data volumes that may be
	// mounted by kubernetes by unmount every mount point under DataDir.
	//
	// Unmount in the reverse order it was mounted so we handle recursive
	// bind mounts and over mounts properly. If we for any reason are not
	// able to unmount, fall back to lazy unmount and if that also fails
	// bail out and don't delete anything.
	//
	// Note that if there are any shared bind mounts under k0s data
	// directory, we may end up unmounting stuff outside the k0s DataDir.
	// If someone has set a bind mount to be shared, we assume that is the
	// desired behavior. See MS_SHARED and NOTES:
	//  - https://man7.org/linux/man-pages/man2/mount.2.html
	//  - https://man7.org/linux/man-pages/man2/umount.2.html#NOTES
	for i := len(procMounts) - 1; i >= 0; i-- {
		v := procMounts[i]
		// avoid unmount datadir if its mounted on separate partition
		// k0s didn't mount it so leave it alone
		if v.Path == d.Config.k0sVars.DataDir {
			dataDirMounted = true
			continue
		}
		if isUnderPath(v.Path, filepath.Join(d.Config.dataDir, "kubelet")) || isUnderPath(v.Path, d.Config.k0sVars.DataDir) {
			logrus.Debugf("%v is mounted! attempting to unmount...", v.Path)
			if err = mounter.Unmount(v.Path); err != nil {
				// if we fail to unmount, try lazy unmount so
				// we don't end up deleting stuff that we
				// shouldn't
				logrus.Warningf("lazy unmounting %v", v.Path)
				if err = UnmountLazy(v.Path); err != nil {
					return fmt.Errorf("failed unmount %v", v.Path)
				}
			}
		}
	}

	if dataDirMounted {
		logrus.Debugf("removing the contents of mounted data-dir (%s)", d.Config.dataDir)
	} else {
		logrus.Debugf("removing k0s generated data-dir (%s)", d.Config.dataDir)
	}

	if err := os.RemoveAll(d.Config.dataDir); err != nil {
		if !dataDirMounted {
			return fmt.Errorf("failed to delete k0s generated data-dir: %w", err)
		}
		if !errorIsUnlinkat(err, d.Config.dataDir) {
			return fmt.Errorf("failed to delete contents of mounted data-dir: %w", err)
		}
	}

	logrus.Debugf("deleting k0s generated run-dir (%s)", d.Config.runDir)
	if err := os.RemoveAll(d.Config.runDir); err != nil {
		return fmt.Errorf("failed to delete %s: %w", d.Config.runDir, err)
	}

	return nil
}

// test if the path is a directory equal to or under base
func isUnderPath(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// this is for checking if the error returned by os.RemoveAll is due to
// it being a mount point. if it is, we can ignore the error. this way
// we can't rely on os.RemoveAll instead of recursively deleting the
// contents of the directory
func errorIsUnlinkat(err error, dir string) bool {
	if err == nil {
		return false
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		return false
	}
	return pathErr.Path == dir && pathErr.Op == "unlinkat"
}
