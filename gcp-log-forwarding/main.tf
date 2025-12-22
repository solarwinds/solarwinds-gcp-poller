terraform {
  required_version = ">= 1.6.0"
  required_providers {
    google  = { source = "hashicorp/google",  version = "~> 6.0" }
    archive = { source = "hashicorp/archive", version = "~> 2.4" }
    random  = { source = "hashicorp/random",  version = "~> 3.6" }
  }
  # backend "gcs" {} # optional
}

provider "google" {
  project = var.project_id
  region  = var.region
}

# ──────────────────────────────────────────────────────────────────────────────
# Project metadata → identities & constants
# ──────────────────────────────────────────────────────────────────────────────
data "google_project" "this" {}

locals {
  project_number      = data.google_project.this.number
  cloud_build_sa      = "${local.project_number}@cloudbuild.gserviceaccount.com"
  compute_default_sa  = "${local.project_number}-compute@developer.gserviceaccount.com"

  # Google-managed bucket used by CFv2 builds to fetch sources
  gcf_sources_bucket  = "gcf-v2-sources-${local.project_number}-${var.region}"

  # Service agents that will invoke the function (via Eventarc → Run)
  eventarc_sa         = "service-${local.project_number}@gcp-sa-eventarc.iam.gserviceaccount.com"
  pubsub_sa           = "service-${local.project_number}@gcp-sa-pubsub.iam.gserviceaccount.com"

  apis = [
    "cloudfunctions.googleapis.com",
    "eventarc.googleapis.com",
    "run.googleapis.com",
    "pubsub.googleapis.com",
    "logging.googleapis.com",
    "cloudbuild.googleapis.com",
    "storage.googleapis.com",
    "artifactregistry.googleapis.com",
  ]

  default_log_filter = <<-EOT
    protoPayload.serviceName="compute.googleapis.com" AND
    logName="projects/${var.project_id}/logs/cloudaudit.googleapis.com%2Factivity" AND
    protoPayload.methodName:(
      "compute.instances.insert" OR
      "compute.instances.delete" OR
      "compute.instances.start"  OR
      "compute.instances.stop"
    )
  EOT
}

# ──────────────────────────────────────────────────────────────────────────────
# Enable required APIs
# ──────────────────────────────────────────────────────────────────────────────
resource "google_project_service" "services" {
  for_each           = toset(local.apis)
  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

# ──────────────────────────────────────────────────────────────────────────────
# Pub/Sub topic (must exist before function trigger)
# ──────────────────────────────────────────────────────────────────────────────
resource "google_pubsub_topic" "logs" {
  name       = var.topic_name
  project    = var.project_id
  depends_on = [google_project_service.services]
}

# ──────────────────────────────────────────────────────────────────────────────
# Staging bucket for function code
# ──────────────────────────────────────────────────────────────────────────────
resource "random_id" "suffix" {
  byte_length = 2
}

resource "google_storage_bucket" "src" {
  name                        = "${var.project_id}-cf-src-${random_id.suffix.hex}"
  project                     = var.project_id
  location                    = var.region
  uniform_bucket_level_access = true
  force_destroy               = true
  depends_on                  = [google_project_service.services]
}

# Allow BOTH potential builders to read your custom staging bucket
resource "google_storage_bucket_iam_member" "cb_can_read_src" {
  bucket = google_storage_bucket.src.name
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${local.cloud_build_sa}"
}

resource "google_storage_bucket_iam_member" "compute_can_read_src" {
  bucket = google_storage_bucket.src.name
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${local.compute_default_sa}"
}

# Google-managed CFv2 sources bucket grants (fixes gcs-fetcher access)
resource "google_storage_bucket_iam_member" "gcf_sources_cb_object_viewer" {
  bucket = local.gcf_sources_bucket
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${local.cloud_build_sa}"
}

resource "google_storage_bucket_iam_member" "gcf_sources_compute_object_viewer" {
  bucket = local.gcf_sources_bucket
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${local.compute_default_sa}"
}

# ──────────────────────────────────────────────────────────────────────────────
# Project-level IAM for Artifact Registry + Logging (for builds)
# ──────────────────────────────────────────────────────────────────────────────
resource "google_project_iam_member" "cb_ar_writer" {
  project = var.project_id
  role    = "roles/artifactregistry.writer"
  member  = "serviceAccount:${local.cloud_build_sa}"
  depends_on = [google_project_service.services]
}

resource "google_project_iam_member" "compute_ar_writer" {
  project = var.project_id
  role    = "roles/artifactregistry.writer"
  member  = "serviceAccount:${local.compute_default_sa}"
  depends_on = [google_project_service.services]
}

resource "google_project_iam_member" "compute_logs_writer" {
  project = var.project_id
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${local.compute_default_sa}"
}

resource "google_project_iam_member" "cb_logs_writer" {
  project = var.project_id
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${local.cloud_build_sa}"
}

# ──────────────────────────────────────────────────────────────────────────────
# Package function code and upload to your staging bucket
# ──────────────────────────────────────────────────────────────────────────────
data "archive_file" "fn_zip" {
  type        = "zip"
  source_dir  = "${path.module}/function_src"
  output_path = "${path.module}/build/function.zip"
}

resource "google_storage_bucket_object" "fn_code" {
  name   = "function-${filesha256(data.archive_file.fn_zip.output_path)}.zip"
  bucket = google_storage_bucket.src.name
  source = data.archive_file.fn_zip.output_path
}

# ──────────────────────────────────────────────────────────────────────────────
# Cloud Function Gen2 (Go 1.23) with Pub/Sub (Eventarc) trigger
# ──────────────────────────────────────────────────────────────────────────────
resource "google_cloudfunctions2_function" "forward_logs" {
  name     = var.function_name
  location = var.region

  build_config {
    runtime     = "go123"
    entry_point = "ForwardLogs"
    source {
      storage_source {
        bucket = google_storage_bucket.src.name
        object = google_storage_bucket_object.fn_code.name
      }
    }
  }

  service_config {
    available_memory   = "${var.fn_memory_mb}Mi"
    timeout_seconds    = var.fn_timeout_seconds
    max_instance_count = var.fn_max_instances
    ingress_settings   = "ALLOW_INTERNAL_ONLY"

    environment_variables = {
      OTLP_ENDPOINT       = var.otlp_endpoint
      API_TOKEN           = var.api_token_value
      LOG_LEVEL           = var.log_level
      MAX_BATCH_RECORDS   = tostring(var.max_batch_records)
      MAX_BATCH_BYTES     = tostring(var.max_batch_bytes)
      EXPORT_TIMEOUT      = var.export_timeout
      MAX_RETRIES         = tostring(var.max_retries)
      WORKERS             = tostring(var.workers)
    }
  }

  event_trigger {
    trigger_region = var.region
    event_type     = "google.cloud.pubsub.topic.v1.messagePublished"
    pubsub_topic   = google_pubsub_topic.logs.id
    retry_policy   = "RETRY_POLICY_RETRY"
  }

  # Ensure build-time IAM exists before building
  depends_on = [
    google_pubsub_topic.logs,
    google_storage_bucket_iam_member.cb_can_read_src,
    google_storage_bucket_iam_member.compute_can_read_src,
    google_storage_bucket_iam_member.gcf_sources_cb_object_viewer,
    google_storage_bucket_iam_member.gcf_sources_compute_object_viewer,
    google_project_iam_member.cb_ar_writer,
    google_project_iam_member.compute_ar_writer,
    google_project_iam_member.compute_logs_writer,
    google_project_iam_member.cb_logs_writer,
  ]
}

# Grant project-wide Cloud Run invoker to the Compute Default SA
resource "google_project_iam_member" "compute_default_run_invoker_project" {
  project = var.project_id
  role    = "roles/run.invoker"
  member  = "serviceAccount:${local.compute_default_sa}"
}

# (Optional) Also allow it to invoke CF Gen2 functions (project-wide)
resource "google_project_iam_member" "compute_default_cf_invoker_project" {
  project = var.project_id
  role    = "roles/cloudfunctions.invoker"
  member  = "serviceAccount:${local.compute_default_sa}"
}


# ──────────────────────────────────────────────────────────────────────────────
# Function-level invoker IAM (CFv2)
# ──────────────────────────────────────────────────────────────────────────────
resource "google_cloudfunctions2_function_iam_member" "eventarc_can_invoke" {
  project        = var.project_id
  location       = var.region
  cloud_function = google_cloudfunctions2_function.forward_logs.name
  role           = "roles/cloudfunctions.invoker"
  member         = "serviceAccount:${local.eventarc_sa}"
  depends_on     = [google_cloudfunctions2_function.forward_logs]
}

resource "google_cloudfunctions2_function_iam_member" "pubsub_can_invoke" {
  project        = var.project_id
  location       = var.region
  cloud_function = google_cloudfunctions2_function.forward_logs.name
  role           = "roles/cloudfunctions.invoker"
  member         = "serviceAccount:${local.pubsub_sa}"
  depends_on     = [google_cloudfunctions2_function.forward_logs]
}

# ──────────────────────────────────────────────────────────────────────────────
# Cloud Run backing service IAM — add run.invoker using the *actual* service id
# ──────────────────────────────────────────────────────────────────────────────
resource "google_cloud_run_v2_service_iam_member" "eventarc_run_invoker" {
  project  = var.project_id
  location = var.region
  # Use the emitted Run service name, not the function name
  name     = google_cloudfunctions2_function.forward_logs.service_config[0].service
  role     = "roles/run.invoker"
  member   = "serviceAccount:${local.eventarc_sa}"
  depends_on = [google_cloudfunctions2_function.forward_logs]
}

resource "google_cloud_run_v2_service_iam_member" "pubsub_run_invoker" {
  project  = var.project_id
  location = var.region
  name     = google_cloudfunctions2_function.forward_logs.service_config[0].service
  role     = "roles/run.invoker"
  member   = "serviceAccount:${local.pubsub_sa}"
  depends_on = [google_cloudfunctions2_function.forward_logs]
}

# ──────────────────────────────────────────────────────────────────────────────
# Log Router sink (AFTER both CF + Run invoker IAM are in place)
# ──────────────────────────────────────────────────────────────────────────────
resource "google_logging_project_sink" "router" {
  name                   = var.sink_name
  project                = var.project_id
  destination            = "pubsub.googleapis.com/${google_pubsub_topic.logs.id}"
  filter                 = coalesce(var.log_filter, local.default_log_filter)
  unique_writer_identity = true

  depends_on = [
    google_cloudfunctions2_function_iam_member.eventarc_can_invoke,
    google_cloudfunctions2_function_iam_member.pubsub_can_invoke,
    google_cloud_run_v2_service_iam_member.eventarc_run_invoker,
    google_cloud_run_v2_service_iam_member.pubsub_run_invoker,
  ]
}

# Allow the sink’s writer identity to publish to the topic (after sink exists)
resource "google_pubsub_topic_iam_member" "sink_publisher" {
  project = var.project_id
  topic   = google_pubsub_topic.logs.name
  role    = "roles/pubsub.publisher"
  member  = google_logging_project_sink.router.writer_identity
  depends_on = [google_logging_project_sink.router]
}
