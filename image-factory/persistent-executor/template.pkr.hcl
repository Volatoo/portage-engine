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

variable "source_vmid" {
  type = number
}

variable "source_template_name" {
  type = string
}

variable "source_image_manifest" {
  type = string
}

variable "source_image_manifest_sha256" {
  type = string
}

variable "template_name" {
  type = string
}

variable "template_description" {
  type = string
}

variable "ssh_username" {
  type    = string
  default = "root"
}

variable "ssh_private_key_file" {
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

variable "executor_template_generation" {
  type = string
}

variable "capacity_pool_id" {
  type = string
}

variable "capacity_provider" {
  type = string
}

variable "execution_zone" {
  type = string
}

variable "architecture" {
  type = string
}

variable "build_mode" {
  type = string
}

variable "profile_id" {
  type = string
}

variable "worker_image_id" {
  type = string
}

variable "worker_image_generation" {
  type = string
}

variable "egress_capability" {
  type    = string
  default = ""
}

variable "portage_server_binary" {
  type = string
}

variable "portage_server_sha256" {
  type = string
}

variable "portage_builder_binary" {
  type = string
}

variable "portage_builder_sha256" {
  type = string
}

variable "terraform_binary" {
  type = string
}

variable "terraform_sha256" {
  type = string
}

variable "terraform_proxmox_provider" {
  type = string
}

variable "terraform_proxmox_provider_sha256" {
  type = string
}

source "proxmox-clone" "persistent_executor" {
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

  tags = "portage-engine;persistent-executor;candidate;${var.executor_template_generation}"
}

build {
  name    = "portage-persistent-executor-${var.executor_template_generation}"
  sources = ["source.proxmox-clone.persistent_executor"]

  provisioner "file" {
    source      = var.portage_server_binary
    destination = "/tmp/portage-server"
  }

  provisioner "file" {
    source      = var.portage_builder_binary
    destination = "/tmp/portage-builder"
  }

  provisioner "file" {
    source      = var.terraform_binary
    destination = "/tmp/terraform"
  }

  provisioner "file" {
    source      = var.terraform_proxmox_provider
    destination = "/tmp/terraform-provider-proxmox.zip"
  }

  provisioner "file" {
    source      = var.source_image_manifest
    destination = "/tmp/source-image-manifest.json"
  }

  provisioner "file" {
    source      = "../../scripts/capacity-executor-identity.sh"
    destination = "/tmp/capacity-executor-identity"
  }

  provisioner "file" {
    source      = "../../deploy/systemd/portage-capacity-executor.service"
    destination = "/tmp/portage-capacity-executor.service"
  }

  provisioner "shell" {
    environment_vars = [
      "PE_EXECUTOR_TEMPLATE_GENERATION=${var.executor_template_generation}",
      "PE_CAPACITY_POOL_ID=${var.capacity_pool_id}",
      "PE_CAPACITY_PROVIDER=${var.capacity_provider}",
      "PE_EXECUTION_ZONE=${var.execution_zone}",
      "PE_ARCHITECTURE=${var.architecture}",
      "PE_BUILD_MODE=${var.build_mode}",
      "PE_PROFILE_ID=${var.profile_id}",
      "PE_WORKER_IMAGE_ID=${var.worker_image_id}",
      "PE_WORKER_IMAGE_GENERATION=${var.worker_image_generation}",
      "PE_EGRESS_CAPABILITY=${var.egress_capability}",
      "PE_PORTAGE_SERVER_SHA256=${var.portage_server_sha256}",
      "PE_PORTAGE_BUILDER_SHA256=${var.portage_builder_sha256}",
      "PE_TERRAFORM_SHA256=${var.terraform_sha256}",
      "PE_TERRAFORM_PROXMOX_PROVIDER_SHA256=${var.terraform_proxmox_provider_sha256}",
      "PE_SOURCE_IMAGE_MANIFEST_SHA256=${var.source_image_manifest_sha256}",
    ]
    scripts = [
      "provision.sh",
      "sanitize-and-gate.sh",
    ]
  }

  post-processor "manifest" {
    output     = "output/${var.template_name}.packer-manifest.json"
    strip_path = true
    custom_data = {
      bootstrap_contract           = "pve-dmi-v1"
      capacity_pool_id             = var.capacity_pool_id
      capacity_provider            = var.capacity_provider
      execution_zone               = var.execution_zone
      architecture                 = var.architecture
      build_mode                   = var.build_mode
      profile_id                   = var.profile_id
      worker_image_id              = var.worker_image_id
      worker_image_generation      = var.worker_image_generation
      executor_template_generation = var.executor_template_generation
      portage_server_sha256        = var.portage_server_sha256
      portage_builder_sha256       = var.portage_builder_sha256
      terraform_sha256             = var.terraform_sha256
      terraform_proxmox_sha256     = var.terraform_proxmox_provider_sha256
      source_template_name         = var.source_template_name
      source_image_manifest_sha256 = var.source_image_manifest_sha256
    }
  }
}
