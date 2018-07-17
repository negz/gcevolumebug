package main

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v1"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

const (
	charset       = "abcdefghijklmnopqrstuvwxyz0123456789"
	statusDone    = "DONE"
	interfaceSCSI = "SCSI"
)

func runID() string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	b := make([]byte, 4)
	for i := range b {
		b[i] = charset[r.Intn(len(charset))]
	}
	return string(b)
}

type disk struct {
	name string
	url  string
}

type tester struct {
	log     *zap.Logger
	c       *compute.Service
	id      string
	project string
	zone    string
}

func (t *tester) Await(name string) error {
	for {
		op, err := t.c.ZoneOperations.Get(t.project, t.zone, name).Do()
		if err != nil {
			return errors.Wrap(err, "cannot check operation")
		}
		if op.Status == statusDone {
			return nil
		}
		if op.Error != nil {
			return errors.New("error waiting for operation")
		}
		time.Sleep(1 * time.Second)
	}
}

func (t *tester) CreateDisks(n int, diskType string, diskSize int64) ([]disk, error) {
	disks := make([]disk, n)

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("gvb-%s-%d", t.id, i)

		l := t.log.With(zap.String("name", name), zap.String("type", diskType))
		l.Info("Creating disk")

		op, err := t.c.Disks.Insert(t.project, t.zone, &compute.Disk{
			Name:   name,
			SizeGb: diskSize,
			Type:   fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/zones/%s/diskTypes/%s", t.project, t.zone, diskType),
		}).Do()
		if err != nil {
			return nil, errors.Wrapf(err, "cannot create GCE disk %s", name)
		}

		if err = t.Await(op.Name); err != nil {
			return nil, errors.Wrap(err, "cannot create GCE disk")
		}
		disks[i] = disk{name: name, url: op.TargetLink}
		l.Info("Created disk", zap.String("url", op.TargetLink))
	}
	return disks, nil
}

func (t *tester) AttachDisks(instance string, disks []disk) {
	for _, d := range disks {
		go func(disk disk) {
			l := t.log.With(zap.String("instance", instance), zap.String("url", disk.url), zap.String("name", disk.name))
			if _, err := t.c.Instances.AttachDisk(t.project, t.zone, instance, &compute.AttachedDisk{
				Source:     disk.url,
				DeviceName: disk.name,
				Interface:  interfaceSCSI,
			}).Do(); err != nil {
				t.log.Error("Disk attachment failed", zap.Error(err))
			}
			l.Info("Attached disk")
		}(d)
	}
}

// MountDisks watches for new disks and attempts to format and mount them using
// the same method as a Linux Kubelet.
func MountDisks(log *zap.Logger, path string, systemd bool) (*fsnotify.Watcher, error) { // nolint:gocyclo
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, errors.Wrap(err, "cannot watch for new disks")
	}

	log.Info("Watching for new disks to mount", zap.String("path", path))

	go func() {
		seen := make(map[string]bool)
		for {
			select {
			case event := <-watcher.Events:
				if event.Op&fsnotify.Create != fsnotify.Create {
					continue
				}

				disk := event.Name
				mp := filepath.Join("/mnt", filepath.Base(disk))
				l := log.With(zap.String("disk", disk), zap.String("mountpoint", mp))

				target, err := filepath.EvalSymlinks(disk)
				if err != nil {
					l.Info("Cannot determine symlink target")
				}
				l = l.With(zap.String("target", target))

				l.Info("New disk detected")
				if seen[target] {
					l.Info("Ignoring previously processed disk")
					continue
				}
				seen[target] = true

				// Create a mountpoint
				if err := os.Mkdir(mp, 0700); err != nil {
					l.Error("Error creating mountpath", zap.Error(err))
					continue
				}
				l.Info("Created mountpath")

				// Format the disk
				fmtcmd := []string{"mkfs.ext4", "-F", "-m0", disk}
				if err := exec.Command(fmtcmd[0], fmtcmd[1:]...).Run(); err != nil { // nolint:gas
					l.Error("Error formatting disk", zap.Error(err), zap.Strings("cmd", fmtcmd))
					continue
				}
				l.Info("Formatted disk")

				// Mount the disk
				mntcmd := []string{"mount", "-t", "ext4", "-o", "rw,seclabel,relatime,data=ordered", disk, mp}
				if systemd {
					sr := []string{"systemd-run", "--scope", "--", "mount", "-t", "ext4", "-o", "rw,seclabel,relatime,data=ordered"}
					mntcmd = append(sr, mntcmd...)
				}
				if err := exec.Command(mntcmd[0], mntcmd[1:]...).Run(); err != nil { // nolint:gas
					l.Error("Error mounting disk", zap.Error(err), zap.Strings("cmd", mntcmd))
					continue
				}
				log.Info("Mounted disk")
			case err := <-watcher.Errors:
				log.Error("Error watching disks", zap.Error(err))
			}
		}
	}()

	if err := watcher.Add(path); err != nil {
		return nil, errors.Wrapf(err, "cannot add %s", path)
	}
	return watcher, nil
}

func main() {
	var (
		app      = kingpin.New(filepath.Base(os.Args[0]), "Attempts to replicate a possible GCE local-ssd bug.").DefaultEnvars()
		debug    = app.Flag("debug", "Run with debug logging.").Short('d').Bool()
		systemd  = app.Flag("use-systemd", "Run mount via systemd-run").Bool()
		diskPath = app.Flag("disk-path", "Path under which new disk devices are created").Default("/dev/disk/by-id").String()
		diskType = app.Flag("disk-type", "Type of disk (pd-standard, pd-ssd)").Default("pd-standard").String()
		diskSize = app.Flag("disk-size", "Size of disks to create in GB").Default("128").Int64()
		disks    = app.Arg("disks", "Number of disks to create and attach").Int()
	)
	kingpin.MustParse(app.Parse(os.Args[1:]))

	log, err := zap.NewProduction()
	if *debug {
		log, err = zap.NewDevelopment()
	}
	kingpin.FatalIfError(err, "cannot create log")
	defer log.Sync()

	id := runID()
	log = log.With(zap.String("id", id))
	log.Info("Starting run")

	client, err := google.DefaultClient(oauth2.NoContext, compute.CloudPlatformScope, compute.ComputeScope)
	kingpin.FatalIfError(err, "cannot create oauth2 client")

	c, err := compute.New(client)
	kingpin.FatalIfError(err, "cannot create GCE service client")

	zone, err := metadata.Zone()
	kingpin.FatalIfError(err, "cannot determine this GCE instance's zone via the metadata endpoint")

	project, err := metadata.ProjectID()
	kingpin.FatalIfError(err, "cannot determine this GCE instance's project via the metadata endpoint")

	instance, err := metadata.InstanceName()
	kingpin.FatalIfError(err, "cannot determine this GCE instance's name via the metadata endpoint")

	// Watch for disks to mount.
	watcher, err := MountDisks(log, *diskPath, *systemd)
	kingpin.FatalIfError(err, "cannot mount GCE disks")
	defer watcher.Close()

	t := tester{log, c, id, project, zone}

	// Create new disks
	created, err := t.CreateDisks(*disks, *diskType, *diskSize)
	kingpin.FatalIfError(err, "cannot create GCE disks")

	// Attach them once they've finished creating
	t.AttachDisks(instance, created)

	exit := make(chan os.Signal)
	signal.Notify(exit, syscall.SIGINT, syscall.SIGTERM)
	<-exit
}
