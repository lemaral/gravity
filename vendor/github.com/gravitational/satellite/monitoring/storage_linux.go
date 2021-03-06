/*
Copyright 2017 Gravitational, Inc.

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

package monitoring

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gravitational/satellite/agent/health"
	pb "github.com/gravitational/satellite/agent/proto/agentpb"
	"github.com/gravitational/satellite/utils"

	sigar "github.com/cloudfoundry/gosigar"
	"github.com/dustin/go-humanize"
	"github.com/gravitational/trace"
	syscall "golang.org/x/sys/unix"
)

// NewStorageChecker creates a new instance of the volume checker
// using the specified checker as configuration
func NewStorageChecker(config StorageConfig) health.Checker {
	return &storageChecker{
		StorageConfig: config,
		osInterface:   &realOS{},
	}
}

// StorageConfig describes checker configuration
type StorageConfig struct {
	// Path represents volume to be checked
	Path string
	// WillBeCreated when true, then all checks will be applied to first existing dir, or fail otherwise
	WillBeCreated bool
	// MinBytesPerSecond is minimum write speed for probe to succeed
	MinBytesPerSecond uint64
	// Filesystems define list of supported filesystems, or any if empty
	Filesystems []string
	// MinFreeBytes define minimum free volume capacity
	MinFreeBytes uint64
}

// storageChecker verifies volume requirements
type storageChecker struct {
	// Config describes the checker configuration
	StorageConfig
	// path refers to the parent directory
	// in case Path does not exist yet
	path string
	osInterface
}

const (
	storageWriteCheckerID = "io-check"
	blockSize             = 1e5
	cycles                = 1024
	stRdonly              = int64(1)
)

// Name returns name of the checker
func (c *storageChecker) Name() string {
	return fmt.Sprintf("%s(%s)", storageWriteCheckerID, c.Path)
}

func (c *storageChecker) Check(ctx context.Context, reporter health.Reporter) {
	err := c.check(ctx, reporter)
	if err != nil {
		reporter.Add(NewProbeFromErr(c.Name(),
			"failed to validate storage requirements", trace.Wrap(err)))
	} else {
		reporter.Add(&pb.Probe{Checker: c.Name(), Status: pb.Probe_Running})
	}
}

func (c *storageChecker) check(ctx context.Context, reporter health.Reporter) error {
	err := c.evalPath()
	if err != nil {
		return trace.Wrap(err)
	}

	return trace.NewAggregate(c.checkFsType(ctx, reporter),
		c.checkCapacity(ctx, reporter),
		c.checkWriteSpeed(ctx, reporter))
}

// cleanPath returns fully evaluated path with symlinks resolved
// if path doesn't exist but should be created, then it returns
// first available parent directory, and checks will be applied to it
func (c *storageChecker) evalPath() error {
	p := c.Path
	for {
		fi, err := os.Stat(p)

		if err != nil && !os.IsNotExist(err) {
			return trace.ConvertSystemError(err)
		}

		if os.IsNotExist(err) && !c.WillBeCreated {
			return trace.BadParameter("%s does not exist", c.Path)
		}

		if err == nil {
			if fi.IsDir() {
				c.path = p
				return nil
			}
			return trace.BadParameter("%s is not a directory", p)
		}

		parent := filepath.Dir(p)
		if parent == p {
			return trace.BadParameter("%s is root and is not a directory", p)
		}
		p = parent
	}
}

func (c *storageChecker) checkFsType(ctx context.Context, reporter health.Reporter) error {
	if len(c.Filesystems) == 0 {
		return nil
	}

	mnt, err := fsFromPath(c.path, c.osInterface)
	if err != nil {
		return trace.Wrap(err)
	}

	probe := &pb.Probe{Checker: c.Name()}

	if utils.StringInSlice(c.Filesystems, mnt.SysTypeName) {
		probe.Status = pb.Probe_Running
	} else {
		probe.Status = pb.Probe_Failed
		probe.Detail = fmt.Sprintf("path %s requires filesystem %v, belongs to %s mount point of type %s",
			c.Path, c.Filesystems, mnt.DirName, mnt.SysTypeName)
	}
	reporter.Add(probe)
	return nil
}

func (c *storageChecker) checkCapacity(ctx context.Context, reporter health.Reporter) error {
	avail, err := c.diskCapacity(c.path)
	if err != nil {
		return trace.Wrap(err)
	}

	if avail < c.MinFreeBytes {
		reporter.Add(&pb.Probe{
			Checker: c.Name(),
			Detail: fmt.Sprintf("%s available space left on %s, minimum of %s required",
				humanize.Bytes(avail), c.Path, humanize.Bytes(c.MinFreeBytes)),
			Status: pb.Probe_Failed,
		})
	}

	return nil
}

func (c *storageChecker) checkWriteSpeed(ctx context.Context, reporter health.Reporter) (err error) {
	if c.MinBytesPerSecond == 0 {
		return
	}

	bps, err := c.diskSpeed(ctx, c.path, "probe")
	if err != nil {
		return trace.Wrap(err)
	}

	if bps >= c.MinBytesPerSecond {
		return nil
	}
	reporter.Add(&pb.Probe{
		Checker: c.Name(),
		Detail: fmt.Sprintf("min write speed %s/sec required, have %s",
			humanize.Bytes(c.MinBytesPerSecond), humanize.Bytes(bps)),
		Status: pb.Probe_Failed,
	})
	return nil
}

type childPathFirst []sigar.FileSystem

func (a childPathFirst) Len() int           { return len(a) }
func (a childPathFirst) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a childPathFirst) Less(i, j int) bool { return strings.HasPrefix(a[i].DirName, a[j].DirName) }

func fsFromPath(path string, mountInfo mountInfo) (*sigar.FileSystem, error) {
	cleanpath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	mounts, err := mountInfo.mounts()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	sort.Sort(childPathFirst(mounts))

	for _, mnt := range mounts {
		// Ignore rootfs mount to find the actual filesystem path is mounted on
		if strings.HasPrefix(cleanpath, mnt.DirName) && mnt.SysTypeName != "rootfs" {
			return &mnt, nil
		}
	}

	return nil, trace.BadParameter("failed to locate filesystem for %s", path)
}

// mounts returns the list of active mounts on the system.
// mounts implements mountInfo
func (r *realMounts) mounts() ([]sigar.FileSystem, error) {
	err := (*sigar.FileSystemList)(r).Get()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return r.List, nil
}

type realOS struct {
	realMounts
}

func (r realOS) diskSpeed(ctx context.Context, path, prefix string) (bps uint64, err error) {
	file, err := ioutil.TempFile(path, prefix)
	if err != nil {
		return 0, trace.ConvertSystemError(err)
	}
	defer file.Close()

	start := time.Now()

	buf := make([]byte, blockSize)
	err = writeN(ctx, file, buf, cycles)
	if err != nil {
		return 0, trace.ConvertSystemError(err)
	}

	if err = file.Sync(); err != nil {
		return 0, trace.ConvertSystemError(err)
	}

	elapsed := time.Since(start).Seconds()
	bps = uint64(blockSize * cycles / elapsed)

	if err = os.Remove(file.Name()); err != nil {
		return 0, trace.ConvertSystemError(err)
	}
	return bps, nil
}

func (r realOS) diskCapacity(path string) (bytesAvail uint64, err error) {
	var stat syscall.Statfs_t

	err = syscall.Statfs(path, &stat)
	if err != nil {
		return 0, trace.Wrap(err)
	}

	bytesAvail = uint64(stat.Bsize) * stat.Bavail
	return bytesAvail, nil
}

func writeN(ctx context.Context, file *os.File, buf []byte, n int) error {
	for i := 0; i < n; i++ {
		_, err := file.Write(buf)
		if err != nil {
			return trace.ConvertSystemError(err)
		}
		if ctx.Err() != nil {
			return trace.Wrap(ctx.Err())
		}
	}
	return nil
}

type realMounts sigar.FileSystemList

type mountInfo interface {
	mounts() ([]sigar.FileSystem, error)
}

type osInterface interface {
	mountInfo
	diskSpeed(ctx context.Context, path, name string) (bps uint64, err error)
	diskCapacity(path string) (bytes uint64, err error)
}
