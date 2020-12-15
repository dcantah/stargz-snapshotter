// +build windows

/*
   Copyright The containerd Authors.

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

package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	winfs "github.com/Microsoft/go-winio/pkg/fs"
	"github.com/Microsoft/hcsshim/pkg/go-runhcs"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/storage"
	"github.com/containerd/continuity/fs"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

const (
	rootfsSizeLabel           = "containerd.io/snapshot/io.microsoft.container.storage.rootfs.size-gb"
	rootfsLocLabel            = "containerd.io/snapshot/io.microsoft.container.storage.rootfs.location"
	reuseScratchLabel         = "containerd.io/snapshot/io.microsoft.container.storage.reuse-scratch"
	reuseScratchOwnerKeyLabel = "containerd.io/snapshot/io.microsoft.owner.key"
)

type snapshotter struct {
	root        string
	ms          *storage.MetaStore
	scratchLock sync.Mutex
	// fs is a filesystem that this snapshotter recognizes.
	fs FileSystem
}

// NewSnapshotter returns a new windows snapshotter
func NewSnapshotter(root string, targetFs FileSystem) (snapshots.Snapshotter, error) {
	if targetFs == nil {
		return nil, errors.New("specify filesystem to use")
	}

	fsType, err := winfs.GetFileSystemType(root)
	if err != nil {
		return nil, err
	}
	if strings.ToLower(fsType) != "ntfs" {
		return nil, errors.Wrapf(errdefs.ErrInvalidArgument, "%s is not on an NTFS volume - only NTFS volumes are supported", root)
	}

	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	ms, err := storage.NewMetaStore(filepath.Join(root, "metadata.db"))
	if err != nil {
		return nil, err
	}

	if err := os.Mkdir(filepath.Join(root, "snapshots"), 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}

	return &snapshotter{
		root: root,
		ms:   ms,
		fs:   targetFs,
	}, nil
}

// Stat returns the info for an active or committed snapshot by name or
// key.
//
// Should be used for parent resolution, existence checks and to discern
// the kind of snapshot.
func (s *snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	ctx, t, err := s.ms.TransactionContext(ctx, false)
	if err != nil {
		return snapshots.Info{}, err
	}
	defer t.Rollback()

	_, info, _, err := storage.GetInfo(ctx, key)
	return info, err
}

func (s *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	ctx, t, err := s.ms.TransactionContext(ctx, true)
	if err != nil {
		return snapshots.Info{}, err
	}
	defer t.Rollback()

	info, err = storage.UpdateInfo(ctx, info, fieldpaths...)
	if err != nil {
		return snapshots.Info{}, err
	}

	if err := t.Commit(); err != nil {
		return snapshots.Info{}, err
	}

	return info, nil
}

func (s *snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	ctx, t, err := s.ms.TransactionContext(ctx, false)
	if err != nil {
		return snapshots.Usage{}, err
	}
	id, info, usage, err := storage.GetInfo(ctx, key)
	t.Rollback() // transaction no longer needed at this point.

	if err != nil {
		return snapshots.Usage{}, err
	}

	if info.Kind == snapshots.KindActive {
		du, err := fs.DiskUsage(ctx, s.getSnapshotDir(id))
		if err != nil {
			return snapshots.Usage{}, err
		}
		usage = snapshots.Usage(du)
	}

	return usage, nil
}

func (s *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	snapshot, err := s.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
	if err != nil {
		return nil, err
	}

	// Try to prepare the remote snapshot. If succeeded, we commit the snapshot now
	// and return ErrAlreadyExists.
	var base snapshots.Info
	for _, opt := range opts {
		if err := opt(&base); err != nil {
			return nil, err
		}
	}
	if target, ok := base.Labels[targetSnapshotLabel]; ok {
		// NOTE: If passed labels include a target of the remote snapshot, `Prepare`
		//       must log whether this method succeeded to prepare that remote snapshot
		//       or not, using the key `remoteSnapshotLogKey` defined in the above. This
		//       log is used by tests in this project.
		lCtx := log.WithLogger(ctx, log.G(ctx).WithField("key", key).WithField("parent", parent))
		if err := s.prepareRemoteSnapshot(ctx, key, base.Labels); err == nil {
			base.Labels[remoteLabel] = fmt.Sprintf("remote snapshot") // Mark this snapshot as remote
			err := s.Commit(ctx, target, key, append(opts, snapshots.WithLabels(base.Labels))...)
			if err == nil {
				log.G(lCtx).WithField(remoteSnapshotLogKey, prepareSucceeded).Debug("prepared remote snapshot")
				return nil, errors.Wrapf(errdefs.ErrAlreadyExists, "target snapshot %q", target)
			}
			log.G(lCtx).WithField(remoteSnapshotLogKey, prepareFailed).
				WithError(err).Debug("failed to internally commit remote snapshot")
		} else {
			log.G(lCtx).WithField(remoteSnapshotLogKey, prepareFailed).
				WithError(err).Debug("failed to prepare remote snapshot")
		}
	}

	return s.mounts(ctx, snapshot, parent)
}

func (s *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	snapshot, err := s.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
	if err != nil {
		return nil, err
	}
	return s.mounts(ctx, snapshot, parent)
}

// prepareRemoteSnapshot tries to prepare the snapshot as a remote snapshot
// using filesystems registered in this snapshotter.
func (s *snapshotter) prepareRemoteSnapshot(ctx context.Context, key string, labels map[string]string) error {
	ctx, t, err := s.ms.TransactionContext(ctx, false)
	if err != nil {
		return err
	}
	defer t.Rollback()
	id, _, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return err
	}
	return s.fs.Mount(ctx, s.getSnapshotDir(id), labels)
}

// Mounts returns the mounts for the transaction identified by key. Can be
// called on an read-write or readonly transaction.
//
// This can be used to recover mounts after calling View or Prepare.
func (s *snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	ctx, t, err := s.ms.TransactionContext(ctx, false)
	if err != nil {
		return nil, err
	}
	defer t.Rollback()

	snapshot, err := storage.GetSnapshot(ctx, key)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get snapshot mount")
	}
	return s.mounts(ctx, snapshot, key)
}

func (s *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	ctx, t, err := s.ms.TransactionContext(ctx, true)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	// grab the existing id
	id, _, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return err
	}

	usage, err := fs.DiskUsage(ctx, s.getSnapshotDir(id))
	if err != nil {
		return err
	}

	if _, err = storage.CommitActive(ctx, key, name, snapshots.Usage(usage), opts...); err != nil {
		return errors.Wrap(err, "failed to commit snapshot")
	}

	return t.Commit()
}

// Remove abandons the transaction identified by key. All resources
// associated with the key will be removed.
func (s *snapshotter) Remove(ctx context.Context, key string) error {
	ctx, t, err := s.ms.TransactionContext(ctx, true)
	if err != nil {
		return err
	}
	defer t.Rollback()

	id, _, err := storage.Remove(ctx, key)
	if err != nil {
		return errors.Wrap(err, "failed to remove")
	}

	path := s.getSnapshotDir(id)
	renamed := s.getSnapshotDir("rm-" + id)
	if err := os.Rename(path, renamed); err != nil && !os.IsNotExist(err) {
		return err
	}

	if err := t.Commit(); err != nil {
		if err1 := os.Rename(renamed, path); err1 != nil {
			// May cause inconsistent data on disk
			log.G(ctx).WithError(err1).WithField("path", renamed).Errorf("Failed to rename after failed commit")
		}
		return errors.Wrap(err, "failed to commit")
	}

	if err := os.RemoveAll(renamed); err != nil {
		// Must be cleaned up, any "rm-*" could be removed if no active transactions
		log.G(ctx).WithError(err).WithField("path", renamed).Warnf("Failed to remove root filesystem")
	}

	return nil
}

// Walk the committed snapshots.
func (s *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, fs ...string) error {
	ctx, t, err := s.ms.TransactionContext(ctx, false)
	if err != nil {
		return err
	}
	defer t.Rollback()

	return storage.WalkInfo(ctx, fn, fs...)
}

// Close closes the snapshotter
func (s *snapshotter) Close() error {
	return s.ms.Close()
}

func (s *snapshotter) mounts(ctx context.Context, sn storage.Snapshot, checkKey string) ([]mount.Mount, error) {
	// Make sure that all layers lower than the target layer are available
	if checkKey != "" && !s.checkAvailability(ctx, checkKey) {
		return nil, errors.Wrapf(errdefs.ErrUnavailable, "layer %q unavailable", sn.ID)
	}

	var (
		roFlag           string
		source           string
		parentLayerPaths []string
	)

	if sn.Kind == snapshots.KindView {
		roFlag = "ro"
	} else {
		roFlag = "rw"
	}

	if len(sn.ParentIDs) == 0 || sn.Kind == snapshots.KindActive {
		source = s.getSnapshotDir(sn.ID)
		parentLayerPaths = s.parentIDsToParentPaths(sn.ParentIDs)
	} else {
		source = s.getSnapshotDir(sn.ParentIDs[0])
		parentLayerPaths = s.parentIDsToParentPaths(sn.ParentIDs[1:])
	}

	// error is not checked here, as a string array will never fail to Marshal
	parentLayersJSON, _ := json.Marshal(parentLayerPaths)
	parentLayersOption := mount.ParentLayerPathsFlag + string(parentLayersJSON)

	var mounts []mount.Mount
	mounts = append(mounts, mount.Mount{
		Source: source,
		Type:   "lcow-layer",
		Options: []string{
			roFlag,
			parentLayersOption,
		},
	})

	return mounts, nil
}

func (s *snapshotter) getSnapshotDir(id string) string {
	return filepath.Join(s.root, "snapshots", id)
}

func (s *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) (storage.Snapshot, error) {
	ctx, t, err := s.ms.TransactionContext(ctx, true)
	if err != nil {
		return storage.Snapshot{}, err
	}

	defer func() {
		if err != nil && t != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	newSnapshot, err := storage.CreateSnapshot(ctx, kind, key, parent, opts...)
	if err != nil {
		return storage.Snapshot{}, errors.Wrap(err, "failed to create snapshot")
	}

	if kind == snapshots.KindActive {
		log.G(ctx).Debug("createSnapshot active")
		// Create the new snapshot dir
		snDir := s.getSnapshotDir(newSnapshot.ID)
		if err := os.MkdirAll(snDir, 0700); err != nil {
			return storage.Snapshot{}, err
		}

		var snapshotInfo snapshots.Info
		for _, o := range opts {
			o(&snapshotInfo)
		}

		defer func() {
			if err != nil {
				os.RemoveAll(snDir)
			}
		}()

		// IO/disk space optimization
		//
		// We only need one sandbox.vhd for the container. Skip making one for this
		// snapshot if this isn't the snapshot that just houses the final sandbox.vhd
		// that will be mounted as the containers scratch. Currently the key for a snapshot
		// where a layer.vhd will be extracted to it will have the substring `extract-` in it.
		// If this is changed this will also need to be changed.
		//
		// We save about 17MB per layer (if the default scratch vhd size of 20GB is used) and of
		// course the time to copy the vhdx per snapshot.
		if !strings.Contains(key, "extract-") {
			// This is the code path that handles re-using a scratch disk that has already been
			// made/mounted for an LCOW UVM. Currently we would create a new disk and mount this
			// into the LCOW UVM for every container but there are certain scenarios where we'd rather
			// just mount a single disk and then have every container share this one storage space instead of
			// every container having it's own xGB of space to play around with.
			//
			// This is accomplished by just making a symlink to the disk that we'd like to share and then
			// using ref counting later on down the stack in hcsshim if we see that we've already mounted this
			// disk.
			shareScratch := snapshotInfo.Labels[reuseScratchLabel]
			ownerKey := snapshotInfo.Labels[reuseScratchOwnerKeyLabel]
			share := shareScratch == "true" && ownerKey != ""
			if share {
				if err = s.handleSharing(ctx, ownerKey, snDir); err != nil {
					return storage.Snapshot{}, err
				}
			} else {
				var sizeGB int
				if sizeGBstr, ok := snapshotInfo.Labels[rootfsSizeLabel]; ok {
					i64, _ := strconv.ParseInt(sizeGBstr, 10, 32)
					sizeGB = int(i64)
				}

				scratchLocation := snapshotInfo.Labels[rootfsLocLabel]
				scratchSource, err := s.openOrCreateScratch(ctx, sizeGB, scratchLocation)
				if err != nil {
					return storage.Snapshot{}, err
				}
				defer scratchSource.Close()

				// Create the sandbox.vhdx for this snapshot from the cache.
				destPath := filepath.Join(snDir, "sandbox.vhdx")
				dest, err := os.OpenFile(destPath, os.O_RDWR|os.O_CREATE, 0700)
				if err != nil {
					return storage.Snapshot{}, errors.Wrap(err, "failed to create sandbox.vhdx in snapshot")
				}
				defer dest.Close()
				if _, err := io.Copy(dest, scratchSource); err != nil {
					dest.Close()
					os.Remove(destPath)
					return storage.Snapshot{}, errors.Wrap(err, "failed to copy cached scratch.vhdx to sandbox.vhdx in snapshot")
				}
			}
		}
	}

	if err := t.Commit(); err != nil {
		return storage.Snapshot{}, errors.Wrap(err, "commit failed")
	}

	return newSnapshot, nil
}

func (s *snapshotter) handleSharing(ctx context.Context, id, snDir string) error {
	var key string
	if err := s.Walk(ctx, func(ctx context.Context, info snapshots.Info) error {
		if strings.Contains(info.Name, id) {
			key = info.Name
		}
		return nil
	}); err != nil {
		return err
	}

	mounts, err := s.Mounts(ctx, key)
	if err != nil {
		return errors.Wrap(err, "failed to get mounts for owner snapshot")
	}

	sandboxPath := filepath.Join(mounts[0].Source, "sandbox.vhdx")
	linkPath := filepath.Join(snDir, "sandbox.vhdx")
	if _, err := os.Stat(sandboxPath); err != nil {
		return errors.Wrap(err, "failed to find sandbox.vhdx in snapshot directory")
	}

	// We've found everything we need, now just make a symlink in our new snapshot to the
	// sandbox.vhdx in the scratch we're asking to share.
	if err := os.Symlink(sandboxPath, linkPath); err != nil {
		return errors.Wrap(err, "failed to create symlink for sandbox scratch space")
	}
	return nil
}

func (s *snapshotter) openOrCreateScratch(ctx context.Context, sizeGB int, scratchLoc string) (_ *os.File, err error) {
	// Create the scratch.vhdx cache file if it doesn't already exit.
	s.scratchLock.Lock()
	defer s.scratchLock.Unlock()

	vhdFileName := "scratch.vhdx"
	if sizeGB > 0 {
		vhdFileName = fmt.Sprintf("scratch_%d.vhdx", sizeGB)
	}

	scratchFinalPath := filepath.Join(s.root, vhdFileName)
	if scratchLoc != "" {
		scratchFinalPath = filepath.Join(scratchLoc, vhdFileName)
	}

	scratchSource, err := os.OpenFile(scratchFinalPath, os.O_RDONLY, 0700)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, errors.Wrapf(err, "failed to open vhd %s for read", vhdFileName)
		}

		log.G(ctx).Debugf("vhd %s not found, creating a new one", vhdFileName)

		// Golang logic for ioutil.TempFile without the file creation
		r := uint32(time.Now().UnixNano() + int64(os.Getpid()))
		r = r*1664525 + 1013904223 // constants from Numerical Recipes

		scratchTempName := fmt.Sprintf("scratch-%s-tmp.vhdx", strconv.Itoa(int(1e9 + r%1e9))[1:])
		scratchTempPath := filepath.Join(s.root, scratchTempName)

		// Create the scratch
		rhcs := runhcs.Runhcs{
			Debug:     true,
			Log:       filepath.Join(s.root, "runhcs-scratch.log"),
			LogFormat: runhcs.JSON,
			Owner:     "teleportd",
		}

		opt := runhcs.CreateScratchOpts{
			SizeGB: sizeGB,
		}

		if err := rhcs.CreateScratchWithOpts(ctx, scratchTempPath, &opt); err != nil {
			os.Remove(scratchTempPath)
			return nil, errors.Wrapf(err, "failed to create '%s' temp file", scratchTempName)
		}
		if err := os.Rename(scratchTempPath, scratchFinalPath); err != nil {
			os.Remove(scratchTempPath)
			return nil, errors.Wrapf(err, "failed to rename '%s' temp file to 'scratch.vhdx'", scratchTempName)
		}
		scratchSource, err = os.OpenFile(scratchFinalPath, os.O_RDONLY, 0700)
		if err != nil {
			os.Remove(scratchFinalPath)
			return nil, errors.Wrap(err, "failed to open scratch.vhdx for read after creation")
		}
	} else {
		log.G(ctx).Debugf("scratch vhd %s was already present. Retrieved from cache", vhdFileName)
	}
	return scratchSource, nil
}

func (s *snapshotter) cleanupSnapshotDirectory(ctx context.Context, dir string) error {
	// We use Filesystem's Unmount API so that it can do necessary finalization
	// before/after the unmount.
	if err := s.fs.Unmount(ctx, dir); err != nil {
		log.G(ctx).WithError(err).WithField("dir", dir).Debug("failed to unmount")
	}
	if err := os.RemoveAll(dir); err != nil {
		return errors.Wrapf(err, "failed to remove directory %q", dir)
	}
	return nil
}

// checkAvailability checks avaiability of the specified layer and all lower
// layers using filesystem's checking functionality.
func (s *snapshotter) checkAvailability(ctx context.Context, key string) bool {
	ctx = log.WithLogger(ctx, log.G(ctx).WithField("key", key))
	log.G(ctx).Debug("checking layer availability")

	ctx, t, err := s.ms.TransactionContext(ctx, false)
	if err != nil {
		log.G(ctx).WithError(err).Warn("failed to get transaction")
		return false
	}
	defer t.Rollback()

	eg, egCtx := errgroup.WithContext(ctx)
	for cKey := key; cKey != ""; {
		id, info, _, err := storage.GetInfo(ctx, cKey)
		if err != nil {
			log.G(ctx).WithError(err).Warnf("failed to get info of %q", cKey)
			return false
		}
		dir := s.getSnapshotDir(id)
		lCtx := log.WithLogger(ctx, log.G(ctx).WithField("mount-point", dir))
		if _, ok := info.Labels[remoteLabel]; ok {
			eg.Go(func() error {
				log.G(lCtx).Debug("checking mount point")
				if err := s.fs.Check(egCtx, dir, info.Labels); err != nil {
					log.G(lCtx).WithError(err).Warn("layer is unavailable")
					return err
				}
				return nil
			})
		} else {
			log.G(lCtx).Debug("layer is normal snapshot(windows-lcow)")
		}
		cKey = info.Parent
	}
	if err := eg.Wait(); err != nil {
		return false
	}
	return true
}

func (s *snapshotter) parentIDsToParentPaths(parentIDs []string) []string {
	var parentLayerPaths []string
	for _, ID := range parentIDs {
		parentLayerPaths = append(parentLayerPaths, s.getSnapshotDir(ID))
	}
	return parentLayerPaths
}
