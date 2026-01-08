variable "project_id" {
  type = string
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "topic_name" {
  type    = string
  default = "vm-events"
}

variable "sink_name" {
  type    = string
  default = "vm-events-sink"
}

# If null, main.tf will construct a default filter using project_id (in locals)
variable "log_filter" {
  type        = string
  default     = null
  description = "Optional custom logging filter; leave null to use main.tf default."
}

variable "function_name" {
  type    = string
  default = "ForwardLogs"
}

variable "fn_memory_mb" {
  type    = number
  default = 512
}

variable "fn_timeout_seconds" {
  type    = number
  default = 60
}

variable "fn_max_instances" {
  type    = number
  default = 10
}

variable "otlp_endpoint" {
  type    = string
  default = "apm.collector.na-01.cloud.solarwinds.com:443"
}

variable "api_token_value" {
  type        = string
  sensitive   = true
  description = "OTLP API token injected as an env var."
}

variable "log_level" {
  type    = string
  default = "INFO"
}

variable "max_batch_records" {
  type    = number
  default = 2000
}

variable "max_batch_bytes" {
  type    = number
  default = 1500000
}

variable "export_timeout" {
  type    = string
  default = "7s"
}

variable "max_retries" {
  type    = number
  default = 3
}

variable "workers" {
  type    = number
  default = 8
}
