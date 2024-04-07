// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:generate packer-sdc struct-markdown
//go:generate packer-sdc mapstructure-to-hcl2 -type Config,nicConfig,diskConfig,vgaConfig,additionalISOsConfig

package proxmoxiso

import (
	"errors"
	"fmt"
	"log"
	"regexp"

	proxmoxcommon "github.com/hashicorp/packer-plugin-proxmox/builder/proxmox/common"
	"github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
)

type Config struct {
	proxmoxcommon.Config `mapstructure:",squash"`

	commonsteps.ISOConfig `mapstructure:",squash"`
	// Path to the ISO file to boot from, expressed as a
	// proxmox datastore path, for example
	// `local:iso/Fedora-Server-dvd-x86_64-29-1.2.iso`.
	// Either `iso_file` OR `iso_url` must be specifed.
	ISOFile string `mapstructure:"iso_file"`
	// Proxmox storage pool onto which to upload
	// the ISO file.
	ISOStoragePool string `mapstructure:"iso_storage_pool"`
	// Download the ISO directly from the PVE node rather than through Packer.
	//
	// Defaults to `false`
	ISODownloadPVE bool `mapstructure:"iso_download_pve"`
	// If true, remove the mounted ISO from the template
	// after finishing. Defaults to `false`.
	UnmountISO      bool `mapstructure:"unmount_iso"`
	shouldUploadISO bool
}

func (c *Config) Prepare(raws ...interface{}) ([]string, []string, error) {
	var errs *packersdk.MultiError
	_, warnings, merrs := c.Config.Prepare(c, raws...)
	if merrs != nil {
		errs = packersdk.MultiErrorAppend(errs, merrs)
	}

	// Check ISO config
	// Either a pre-uploaded ISO should be referenced in iso_file, OR a URL
	// (possibly to a local file) to an ISO file that will be downloaded and
	// then uploaded to Proxmox.
	// If iso_download_pve is true, iso_url will be downloaded directly to the
	// PVE node.
	if c.ISOFile != "" {
		c.shouldUploadISO = false
	} else {
		isoWarnings, isoErrors := c.ISOConfig.Prepare(&c.Ctx)
		errs = packersdk.MultiErrorAppend(errs, isoErrors...)
		warnings = append(warnings, isoWarnings...)
		c.shouldUploadISO = true
	}

	if (c.ISOFile == "" && len(c.ISOConfig.ISOUrls) == 0) || (c.ISOFile != "" && len(c.ISOConfig.ISOUrls) != 0) {
		errs = packersdk.MultiErrorAppend(errs, errors.New("either iso_file or iso_url, but not both, must be specified"))
	}
	if len(c.ISOConfig.ISOUrls) != 0 && c.ISOStoragePool == "" {
		errs = packersdk.MultiErrorAppend(errs, errors.New("when specifying iso_url, iso_storage_pool must also be specified"))
	}

	if c.Agent != config.TriFalse {
		c.Agent = config.TriTrue
	}

	// if c.VMName == "" {
	// 	// Default to packer-[time-ordered-uuid]
	// 	c.VMName = fmt.Sprintf("packer-%s", uuid.TimeOrderedUUID())
	// }
	if c.Memory < 16 {
		log.Printf("Memory %d is too small, using default: 512", c.Memory)
		c.Memory = 512
	}
	if c.Memory < c.BalloonMinimum {
		errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("ballooning_minimum (%d) must be lower than memory (%d)", c.BalloonMinimum, c.Memory))
	}
	if c.Cores < 1 {
		log.Printf("Number of cores %d is too small, using default: 1", c.Cores)
		c.Cores = 1
	}
	if c.Sockets < 1 {
		log.Printf("Number of sockets %d is too small, using default: 1", c.Sockets)
		c.Sockets = 1
	}
	if c.CPUType == "" {
		log.Printf("CPU type not set, using default 'kvm64'")
		c.CPUType = "kvm64"
	}
	if c.OS == "" {
		log.Printf("OS not set, using default 'other'")
		c.OS = "other"
	}

	for idx, disk := range c.Disks {
		if disk.Type == "" {
			log.Printf("Disk %d type not set, using default 'scsi'", idx)
			c.Disks[idx].Type = "scsi"
		}
		if disk.Size == "" {
			log.Printf("Disk %d size not set, using default '20G'", idx)
			c.Disks[idx].Size = "20G"
		}
		if disk.CacheMode == "" {
			log.Printf("Disk %d cache mode not set, using default 'none'", idx)
			c.Disks[idx].CacheMode = "none"
		}
		if disk.IOThread {
			// io thread is only supported by virtio-scsi-single controller
			if c.SCSIController != "virtio-scsi-single" {
				errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("io thread option requires virtio-scsi-single controller"))
			} else {
				// ... and only for virtio and scsi disks
				if !(disk.Type == "scsi" || disk.Type == "virtio") {
					errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("io thread option requires scsi or a virtio disk"))
				}
			}
		}
		if disk.SSD && disk.Type == "virtio" {
			errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("SSD emulation is not supported on virtio disks"))
		}
		if disk.StoragePool == "" {
			errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("disks[%d].storage_pool must be specified", idx))
		}
		if disk.StoragePoolType != "" {
			warnings = append(warnings, "storage_pool_type is deprecated and should be omitted, it will be removed in a later version of the proxmox plugin")
		}
	}
	if len(c.Serials) > 4 {
		errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("too many serials: %d serials defined, but proxmox accepts 4 elements maximum", len(c.Serials)))
	}
	res := regexp.MustCompile(`^(/dev/.+|socket)$`)
	for _, serial := range c.Serials {
		if !res.MatchString(serial) {
			errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("serials must respond to pattern \"/dev/.+\" or be \"socket\". It was \"%s\"", serial))
		}
	}
	if c.SCSIController == "" {
		log.Printf("SCSI controller not set, using default 'lsi'")
		c.SCSIController = "lsi"
	}

	if errs != nil && len(errs.Errors) > 0 {
		return nil, warnings, errs
	}
	return nil, warnings, nil
}
