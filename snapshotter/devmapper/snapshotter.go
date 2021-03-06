// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package devmapper

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/storage"
	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/firecracker-microvm/firecracker-containerd/snapshotter/pkg/dmsetup"
)

const (
	metadataFileName = "metadata.db"
	fsTypeExt4       = "ext4"
)

type closeFunc func() error

// Snapshotter implements containerd's snapshotter (https://godoc.org/github.com/containerd/containerd/snapshots#Snapshotter)
// based on Linux device-mapper targets.
type Snapshotter struct {
	store     *storage.MetaStore
	pool      *PoolDevice
	config    *Config
	cleanupFn []closeFunc
	closeOnce sync.Once
}

// NewSnapshotter creates new device mapper snapshotter.
// Internally it creates thin-pool device (or reloads if it's already exists) and
// initializes a database file for metadata.
func NewSnapshotter(ctx context.Context, configPath string) (*Snapshotter, error) {
	log.G(ctx).WithField("config-path", configPath).Info("creating devmapper snapshotter")

	var cleanupFn []closeFunc

	config, err := LoadConfig(configPath)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(config.RootPath, 0750); err != nil && !os.IsExist(err) {
		return nil, errors.Wrapf(err, "failed to create root directory: %s", config.RootPath)
	}

	store, err := storage.NewMetaStore(filepath.Join(config.RootPath, metadataFileName))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create metastore")
	}

	cleanupFn = append(cleanupFn, store.Close)

	poolDevice, err := NewPoolDevice(ctx, config)
	if err != nil {
		return nil, err
	}

	cleanupFn = append(cleanupFn, poolDevice.Close)

	return &Snapshotter{
		store:     store,
		config:    config,
		pool:      poolDevice,
		cleanupFn: cleanupFn,
	}, nil
}

// Stat returns the info for an active or committed snapshot from store
func (s *Snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	log.G(ctx).WithField("key", key).Debug("stat")

	var (
		info snapshots.Info
		err  error
	)

	err = s.withTransaction(ctx, false, func(ctx context.Context) error {
		_, info, _, err = storage.GetInfo(ctx, key)
		return err
	})

	return info, err
}

// Update updates an existing snapshot info's data
func (s *Snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	log.G(ctx).Debugf("update: %s", strings.Join(fieldpaths, ", "))

	var err error
	err = s.withTransaction(ctx, true, func(ctx context.Context) error {
		info, err = storage.UpdateInfo(ctx, info, fieldpaths...)
		return err
	})

	return info, err
}

// Usage not yet implemented
func (s *Snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	log.G(ctx).WithField("key", key).Debug("usage")

	return snapshots.Usage{}, errors.New("usage not implemented")
}

// Mounts return the list of mounts for the active or view snapshot
func (s *Snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	log.G(ctx).WithField("key", key).Debug("mounts")

	var (
		snap storage.Snapshot
		err  error
	)

	err = s.withTransaction(ctx, false, func(ctx context.Context) error {
		snap, err = storage.GetSnapshot(ctx, key)
		return err
	})

	return s.buildMounts(snap), nil
}

// Prepare creates thin device for an active snapshot identified by key
func (s *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).WithFields(logrus.Fields{"key": key, "parent": parent}).Debug("prepare")

	var (
		mounts []mount.Mount
		err    error
	)

	err = s.withTransaction(ctx, true, func(ctx context.Context) error {
		mounts, err = s.createSnapshot(ctx, snapshots.KindActive, key, parent, opts...)
		return err
	})

	return mounts, err
}

// View creates readonly thin device for the given snapshot key
func (s *Snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).WithFields(logrus.Fields{"key": key, "parent": parent}).Debug("prepare")

	var (
		mounts []mount.Mount
		err    error
	)

	err = s.withTransaction(ctx, true, func(ctx context.Context) error {
		mounts, err = s.createSnapshot(ctx, snapshots.KindView, key, parent, opts...)
		return err
	})

	return mounts, err
}

// Commit marks an active snapshot as committed in meta store.
// Block device unmount operation captures snapshot changes by itself, so no
// additional actions needed within Commit operation.
func (s *Snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	log.G(ctx).WithFields(logrus.Fields{"name": name, "key": key}).Debug("commit")

	return s.withTransaction(ctx, true, func(ctx context.Context) error {
		_, err := storage.CommitActive(ctx, key, name, snapshots.Usage{}, opts...)
		return err
	})
}

// Remove removes thin device and snapshot metadata by key
func (s *Snapshotter) Remove(ctx context.Context, key string) error {
	log.G(ctx).WithField("key", key).Debug("remove")

	return s.withTransaction(ctx, true, func(ctx context.Context) error {
		return s.removeDevice(ctx, key)
	})
}

func (s *Snapshotter) removeDevice(ctx context.Context, key string) error {
	snapID, _, err := storage.Remove(ctx, key)
	if err != nil {
		return err
	}

	deviceName := s.getDeviceName(snapID)
	if err := s.pool.RemoveDevice(ctx, deviceName, true); err != nil {
		log.G(ctx).WithError(err).Errorf("failed to remove device")
		return err
	}

	return nil
}

// Walk iterates through all metadata Info for the stored snapshots and calls the provided function for each.
func (s *Snapshotter) Walk(ctx context.Context, fn func(context.Context, snapshots.Info) error) error {
	log.G(ctx).Debug("walk")
	return s.withTransaction(ctx, false, func(ctx context.Context) error {
		return storage.WalkInfo(ctx, fn)
	})
}

// Close releases devmapper snapshotter resources.
// All subsequent Close calls will be ignored.
func (s *Snapshotter) Close() error {
	log.L.Debug("close")

	var result *multierror.Error
	s.closeOnce.Do(func() {
		for _, fn := range s.cleanupFn {
			if err := fn(); err != nil {
				result = multierror.Append(result, err)
			}
		}
	})

	return result.ErrorOrNil()
}

func (s *Snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	snap, err := storage.CreateSnapshot(ctx, kind, key, parent, opts...)
	if err != nil {
		return nil, err
	}

	if len(snap.ParentIDs) == 0 {
		deviceName := s.getDeviceName(snap.ID)
		log.G(ctx).Debugf("creating new thin device '%s'", deviceName)

		err := s.pool.CreateThinDevice(ctx, deviceName, s.config.BaseImageSizeBytes)
		if err != nil {
			log.G(ctx).WithError(err).Errorf("failed to create thin device for snapshot %s", snap.ID)
			return nil, err
		}

		if err := s.mkfs(ctx, deviceName); err != nil {
			return nil, err
		}
	} else {
		parentDeviceName := s.getDeviceName(snap.ParentIDs[0])
		snapDeviceName := s.getDeviceName(snap.ID)
		log.G(ctx).Debugf("creating snapshot device '%s' from '%s'", snapDeviceName, parentDeviceName)

		err := s.pool.CreateSnapshotDevice(ctx, parentDeviceName, snapDeviceName, s.config.BaseImageSizeBytes)
		if err != nil {
			log.G(ctx).WithError(err).Errorf("failed to create snapshot device from parent %s", parentDeviceName)
			return nil, err
		}
	}

	mounts := s.buildMounts(snap)

	// Remove default directories not expected by the container image
	_ = mount.WithTempMount(ctx, mounts, func(root string) error {
		return os.Remove(filepath.Join(root, "lost+found"))
	})

	return mounts, nil
}

// mkfs creates ext4 filesystem on the given devmapper device
func (s *Snapshotter) mkfs(ctx context.Context, deviceName string) error {
	args := []string{
		"-E",
		// We don't want any zeroing in advance when running mkfs on thin devices (see "man mkfs.ext4")
		"nodiscard,lazy_itable_init=0,lazy_journal_init=0",
		dmsetup.GetFullDevicePath(deviceName),
	}

	log.G(ctx).Debugf("mkfs.ext4 %s", strings.Join(args, " "))
	output, err := exec.Command("mkfs.ext4", args...).CombinedOutput()
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to write fs:\n%s", string(output))
		return err
	}

	log.G(ctx).Debugf("mkfs:\n%s", string(output))
	return nil
}

func (s *Snapshotter) getDeviceName(snapID string) string {
	// Add pool name as prefix to avoid collisions with devices from other pools
	return fmt.Sprintf("%s-snap-%s", s.config.PoolName, snapID)
}

func (s *Snapshotter) getDevicePath(snap storage.Snapshot) string {
	name := s.getDeviceName(snap.ID)
	return dmsetup.GetFullDevicePath(name)
}

func (s *Snapshotter) buildMounts(snap storage.Snapshot) []mount.Mount {
	var options []string

	if snap.Kind != snapshots.KindActive {
		options = append(options, "ro")
	}

	mounts := []mount.Mount{
		{
			Source:  s.getDevicePath(snap),
			Type:    fsTypeExt4,
			Options: options,
		},
	}

	return mounts
}

// withTransaction wraps fn callback with containerd's meta store transaction.
// If callback returns an error or transaction is not writable, database transaction will be discarded.
func (s *Snapshotter) withTransaction(ctx context.Context, writable bool, fn func(ctx context.Context) error) error {
	ctx, trans, err := s.store.TransactionContext(ctx, writable)
	if err != nil {
		return err
	}

	var result *multierror.Error

	err = fn(ctx)
	if err != nil {
		result = multierror.Append(result, err)
	}

	// Always rollback if transaction is not writable
	if err != nil || !writable {
		if terr := trans.Rollback(); terr != nil {
			log.G(ctx).WithError(terr).Error("failed to rollback transaction")
			result = multierror.Append(result, errors.Wrap(terr, "rollback failed"))
		}
	} else {
		if terr := trans.Commit(); terr != nil {
			log.G(ctx).WithError(terr).Error("failed to commit transaction")
			result = multierror.Append(result, errors.Wrap(terr, "commit failed"))
		}
	}

	if err := result.ErrorOrNil(); err != nil {
		log.G(ctx).WithError(err).Debug("snapshotter error")

		// Unwrap if just one error
		if result.Len() == 1 {
			return result.Errors[0]
		}

		return err
	}

	return nil
}
