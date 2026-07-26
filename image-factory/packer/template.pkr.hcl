packer {
  required_version = "= 1.15.4"
  required_plugins {
    proxmox = {
      source  = "github.com/hashicorp/proxmox"
      version = "= 1.2.3"
    }
  }
}

variable "proxmox_url" {
  type = string
}

variable "proxmox_username" {
  type      = string
  sensitive = true
}

variable "proxmox_token" {
  type      = string
  sensitive = true
}

variable "proxmox_node" {
  type = string
}

variable "proxmox_pool" {
  type = string
}

variable "proxmox_storage" {
  type = string
}

variable "proxmox_bridge" {
  type    = string
  default = "vmbr0"
}

variable "proxmox_insecure" {
  type    = bool
  default = false
}

variable "ssh_username" {
  type    = string
  default = "root"
}

variable "ssh_private_key_file" {
  type = string
}

variable "source_vmid" {
  type = number
}

variable "template_name" {
  type = string
}

variable "template_description" {
  type = string
}

variable "cores" {
  type    = number
  default = 4
}

variable "memory" {
  type    = number
  default = 8192
}

variable "profile_id" {
  type = string
}

variable "profile_path" {
  type = string
}

variable "profile_repository" {
  type = string
}

variable "profile_parents" {
  type = list(string)
}

variable "image_generation" {
  type = string
}

variable "mirror_bundle_id" {
  type = string
}

variable "repository_names" {
  type = list(string)
}

variable "repository_uris" {
  type = list(string)
}

variable "repository_revisions" {
  type = list(string)
}

variable "gentoo_mirror" {
  type = string
}

variable "binhost" {
  type    = string
  default = ""
}

variable "packages" {
  type = list(string)
}

variable "package_sets" {
  type = list(string)
}

variable "package_set_catalog_digest" {
  type = string
}

variable "desktop" {
  type    = bool
  default = false
}

variable "display_model" {
  type = string
  validation {
    condition     = contains(["std", "qxl"], var.display_model)
    error_message = "Display model must be std or qxl."
  }
}

variable "build_plan_path" {
  type = string
}

variable "packer_manifest_path" {
  type = string
}

variable "plan_digest" {
  type = string
}

variable "input_lock_digest" {
  type = string
}

variable "common_config_digest" {
  type = string
}

variable "source_template" {
  type = string
}

variable "source_provenance_object_id" {
  type = string
}

variable "source_provenance_digest" {
  type = string
}

variable "repository_bundle_paths" {
  type = list(string)
}

variable "repository_bundle_names" {
  type = list(string)
}

variable "locked_repository_input_paths" {
  type = list(string)
}

variable "profile_repository_key_name" {
  type    = string
  default = ""
}

variable "gentoo_repository_key_name" {
  type = string
}

variable "trusted_ca_name" {
  type    = string
  default = ""
}

variable "distfile_manifest_path" {
  type = string
}

source "proxmox-clone" "gentoo" {
  proxmox_url              = var.proxmox_url
  username                 = var.proxmox_username
  token                    = var.proxmox_token
  node                     = var.proxmox_node
  pool                     = var.proxmox_pool
  insecure_skip_tls_verify = var.proxmox_insecure

  clone_vm_id          = var.source_vmid
  full_clone           = true
  vm_name              = var.template_name
  template_name        = var.template_name
  template_description = var.template_description

  os              = "l26"
  bios            = "ovmf"
  machine         = "q35"
  cores           = var.cores
  sockets         = 1
  memory          = var.memory
  cpu_type        = "host"
  qemu_agent      = true
  scsi_controller = "virtio-scsi-single"

  vga {
    type   = var.display_model
    memory = 64
  }

  cloud_init              = true
  cloud_init_storage_pool = var.proxmox_storage
  cloud_init_disk_type    = "scsi"

  network_adapters {
    model    = "virtio"
    bridge   = var.proxmox_bridge
    firewall = true
  }

  ssh_username         = var.ssh_username
  ssh_private_key_file = var.ssh_private_key_file
  ssh_timeout          = "20m"
  task_timeout         = "30m"

  tags = "portage-engine;image-factory-build;candidate;${var.image_generation}"
}

build {
  name    = "portage-engine-${var.image_generation}"
  sources = ["source.proxmox-clone.gentoo"]

  provisioner "file" {
    source      = var.build_plan_path
    destination = "/tmp/pe-build-plan.json"
  }

  provisioner "shell" {
    inline = ["install -d -m 0700 /tmp/pe-repository-bundles"]
  }

  provisioner "file" {
    sources     = var.locked_repository_input_paths
    destination = "/tmp/pe-repository-bundles/"
  }

  provisioner "file" {
    source      = var.distfile_manifest_path
    destination = "/tmp/distfiles.MANIFEST.json"
  }

  provisioner "file" {
    source      = "scripts/hydrate-distfiles.py"
    destination = "/tmp/hydrate-distfiles.py"
  }

  provisioner "file" {
    source      = "../catalyst/verify-profile.py"
    destination = "/tmp/verify-profile.py"
  }

  provisioner "file" {
    source      = "../desktop/guest-agent.py"
    destination = "/tmp/portage-desktop-agent.py"
  }

  provisioner "shell" {
    environment_vars = [
      "PE_PROFILE_ID=${var.profile_id}",
      "PE_PROFILE_PATH=${var.profile_path}",
      "PE_PROFILE_REPOSITORY=${var.profile_repository}",
      "PE_PROFILE_PARENTS=${join(",", var.profile_parents)}",
      "PE_IMAGE_GENERATION=${var.image_generation}",
      "PE_MIRROR_BUNDLE_ID=${var.mirror_bundle_id}",
      "PE_REPOSITORY_NAMES=${join(",", var.repository_names)}",
      "PE_REPOSITORY_URIS=${join(",", var.repository_uris)}",
      "PE_REPOSITORY_REVISIONS=${join(",", var.repository_revisions)}",
      "PE_REPOSITORY_BUNDLE_NAMES=${join(",", var.repository_bundle_names)}",
      "PE_PROFILE_REPOSITORY_KEY_NAME=${var.profile_repository_key_name}",
      "PE_GENTOO_REPOSITORY_KEY_NAME=${var.gentoo_repository_key_name}",
      "PE_TRUSTED_CA_NAME=${var.trusted_ca_name}",
      "PE_GENTOO_MIRROR=${var.gentoo_mirror}",
      "PE_BINHOST=${var.binhost}",
      "PE_PACKAGES=${join(",", var.packages)}",
      "PE_PACKAGE_SETS=${join(",", var.package_sets)}",
      "PE_PACKAGE_SET_CATALOG_DIGEST=${var.package_set_catalog_digest}",
      "PE_DESKTOP=${var.desktop ? "true" : "false"}",
      "PE_DISPLAY_MODEL=${var.display_model}",
      "PE_BUILD_PLAN_DIGEST=${var.plan_digest}",
      "PE_INPUT_LOCK_DIGEST=${var.input_lock_digest}",
      "PE_COMMON_CONFIG_DIGEST=${var.common_config_digest}",
      "PE_SOURCE_TEMPLATE=${var.source_template}",
      "PE_SOURCE_VMID=${var.source_vmid}",
      "PE_SOURCE_PROVENANCE_OBJECT_ID=${var.source_provenance_object_id}",
      "PE_SOURCE_PROVENANCE_DIGEST=${var.source_provenance_digest}",
    ]
    scripts = [
      "scripts/provision.sh",
      "scripts/sanitize-and-gate.sh",
    ]
  }

  post-processor "manifest" {
    output     = var.packer_manifest_path
    strip_path = true
    custom_data = {
      image_generation            = var.image_generation
      build_plan_digest           = var.plan_digest
      input_lock_digest           = var.input_lock_digest
      common_config_digest        = var.common_config_digest
      mirror_bundle_id            = var.mirror_bundle_id
      desktop                     = var.desktop ? "true" : "false"
      display_model               = var.display_model
      profile_id                  = var.profile_id
      profile_path                = var.profile_path
      profile_repository          = var.profile_repository
      profile_parents             = join(",", var.profile_parents)
      package_sets                = join(",", var.package_sets)
      package_set_catalog_digest  = var.package_set_catalog_digest
      repository_names            = join(",", var.repository_names)
      repository_revisions        = join(",", var.repository_revisions)
      source_provenance_digest    = var.source_provenance_digest
      source_provenance_object_id = var.source_provenance_object_id
      source_template             = var.source_template
      source_vmid                 = "${var.source_vmid}"
      template_name               = var.template_name
    }
  }
}
