package main

import (
	"errors"
	"fmt"
	"github.com/docker/go-units"
	"github.com/debos/fakemachine"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
)

type Partition struct {
	number int
	Name   string
	Start  string
	End    string
	FS     string
	Flags  []string
	FSUUID string
}

type Mountpoint struct {
	Mountpoint string
	Partition  string
	Options    []string
	part       *Partition
}

type ImagePartitionAction struct {
	BaseAction    `yaml:",inline"`
	ImageName     string
	ImageSize     string
	PartitionType string
	Partitions    []Partition
	Mountpoints   []Mountpoint
	size          int64
	usingLoop     bool
}

func (i *ImagePartitionAction) generateFSTab(context *DebosContext) error {
	context.imageFSTab.Reset()

	for _, m := range i.Mountpoints {
		options := []string{"defaults"}
		options = append(options, m.Options...)
		if m.part.FSUUID == "" {
			return fmt.Errorf("Missing fs UUID for partition %s!?!", m.part.Name)
		}
		context.imageFSTab.WriteString(fmt.Sprintf("UUID=%s\t%s\t%s\t%s\t0\t0\n",
			m.part.FSUUID, m.Mountpoint, m.part.FS,
			strings.Join(options, ",")))
	}

	return nil
}

func (i *ImagePartitionAction) generateKernelRoot(context *DebosContext) error {
	for _, m := range i.Mountpoints {
		if m.Mountpoint == "/" {
			if m.part.FSUUID == "" {
				return errors.New("No fs UUID for root partition !?!")
			}
			context.imageKernelRoot = fmt.Sprintf("root=UUID=%s", m.part.FSUUID)
			break
		}
	}

	return nil
}

func (i ImagePartitionAction) getPartitionDevice(number int, context DebosContext) string {
	/* If the iamge device has a digit as the last character, the partition
	 * suffix is p<number> else it's just <number> */
	last := context.image[len(context.image)-1]
	if last >= '0' && last <= '9' {
		return fmt.Sprintf("%sp%d", context.image, number)
	} else {
		return fmt.Sprintf("%s%d", context.image, number)
	}
}

func (i ImagePartitionAction) PreMachine(context *DebosContext, m *fakemachine.Machine,
	args *[]string) error {
	err := m.CreateImage(i.ImageName, i.size)
	if err != nil {
		return err
	}

	context.image = "/dev/vda"
	*args = append(*args, "--internal-image", "/dev/vda")
	return nil
}

func (i ImagePartitionAction) formatPartition(p *Partition, context DebosContext) error {
	label := fmt.Sprintf("Formatting partition %d", p.number)
	path := i.getPartitionDevice(p.number, context)

	cmdline := []string{}
	switch p.FS {
	case "fat32":
		cmdline = append(cmdline, "mkfs.vfat", "-n", p.Name)
	default:
		cmdline = append(cmdline, fmt.Sprintf("mkfs.%s", p.FS), "-L", p.Name)
	}
	cmdline = append(cmdline, path)

	Command{}.Run(label, cmdline...)

	uuid, err := exec.Command("blkid", "-o", "value", "-s", "UUID", "-p", "-c", "none", path).Output()
	if err != nil {
		return fmt.Errorf("Failed to get uuid: %s", err)
	}
	p.FSUUID = strings.TrimSpace(string(uuid[:]))

	return nil
}

func (i ImagePartitionAction) PreNoMachine(context *DebosContext) error {

	img, err := os.OpenFile(i.ImageName, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		return fmt.Errorf("Couldn't open image file: %v", err)
	}

	err = img.Truncate(i.size)
	if err != nil {
		return fmt.Errorf("Couldn't resize image file: %v", err)
	}

	img.Close()

	loop, err := exec.Command("losetup", "-f", "--show", i.ImageName).Output()
	if err != nil {
		return fmt.Errorf("Failed to setup loop device")
	}
	context.image = strings.TrimSpace(string(loop[:]))
	i.usingLoop = true

	return nil
}

func (i ImagePartitionAction) Run(context *DebosContext) error {
	i.LogStart()
	err := Command{}.Run("parted", "parted", "-s", context.image, "mklabel", i.PartitionType)
	if err != nil {
		return err
	}
	for idx, _ := range i.Partitions {
		p := &i.Partitions[idx]
		var name string
		if i.PartitionType == "gpt" {
			name = p.Name
		} else {
			name = "primary"
		}
		err = Command{}.Run("parted", "parted", "-a", "none", "-s", context.image, "mkpart",
			name, p.FS, p.Start, p.End)
		if err != nil {
			return err
		}

		if p.Flags != nil {
			for _, flag := range p.Flags {
				err = Command{}.Run("parted", "parted", "-s", context.image, "set",
					fmt.Sprintf("%d", p.number), flag, "on")
				if err != nil {
					return err
				}
			}
		}

		err = i.formatPartition(p, *context)
		if err != nil {
			return err
		}
	}

	context.imageMntDir = path.Join(context.scratchdir, "mnt")
	os.MkdirAll(context.imageMntDir, 755)
	for _, m := range i.Mountpoints {
		dev := i.getPartitionDevice(m.part.number, *context)
		mntpath := path.Join(context.imageMntDir, m.Mountpoint)
		os.MkdirAll(mntpath, 755)
		var fs string
		switch m.part.FS {
		case "fat32":
			fs = "vfat"
		default:
			fs = m.part.FS
		}
		err := syscall.Mount(dev, mntpath, fs, 0, "")
		if err != nil {
			return fmt.Errorf("%s mount failed: %v", m.part.Name, err)
		}
	}

	err = i.generateFSTab(context)
	if err != nil {
		return err
	}

	err = i.generateKernelRoot(context)
	if err != nil {
		return err
	}

	return nil
}

func (i ImagePartitionAction) Cleanup(context DebosContext) error {
	for idx := len(i.Mountpoints) - 1; idx >= 0; idx-- {
		m := i.Mountpoints[idx]
		mntpath := path.Join(context.imageMntDir, m.Mountpoint)
		syscall.Unmount(mntpath, 0)
	}

	if i.usingLoop {
		exec.Command("losetup", "-d", context.image).Run()
	}

	return nil
}

func (i *ImagePartitionAction) Verify(context *DebosContext) error {
	num := 1
	for idx, _ := range i.Partitions {
		p := &i.Partitions[idx]
		p.number = num
		num++
		if p.Name == "" {
			return fmt.Errorf("Partition without a name")
		}
		if p.Start == "" {
			return fmt.Errorf("Partition %s missing start", p.Name)
		}
		if p.End == "" {
			return fmt.Errorf("Partition %s missing end", p.Name)
		}

		if p.FS == "" {
			return fmt.Errorf("Partition %s missing fs type", p.Name)
		}
	}

	for idx, _ := range i.Mountpoints {
		m := &i.Mountpoints[idx]
		for pidx, _ := range i.Partitions {
			p := &i.Partitions[pidx]
			if m.Partition == p.Name {
				m.part = p
				break
			}
		}
		if m.part == nil {
			return fmt.Errorf("Couldn't fount partition for %s", m.Mountpoint)
		}
	}

	size, err := units.FromHumanSize(i.ImageSize)
	if err != nil {
		return fmt.Errorf("Failed to parse image size: %s", i.ImageSize)
	}

	i.size = size
	return nil
}
