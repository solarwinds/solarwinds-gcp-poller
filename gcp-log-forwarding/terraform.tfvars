# terraform.tfvars

project_id      = "solarwindsgcpproject"
region          = "us-central1"

# Pub/Sub + Sink
topic_name      = "solarwinds-gcp-events"
sink_name       = "solarwinds-gcp-events-sink"

# Log filter
log_filter = <<-EOT
  protoPayload.serviceName="compute.googleapis.com" AND
  logName="projects/your-gcp-project-id/logs/cloudaudit.googleapis.com%2Factivity" AND
  protoPayload.methodName=(
    "v1.compute.instances.insert" OR
    "v1.compute.instances.delete" OR
    "v1.compute.instances.start"  OR
    "v1.compute.instances.stop"
  )
EOT

# Function + OTLP
function_name   = "ForwardLogs"
otlp_endpoint   = "otel.collector.na-01.dev-ssp.solarwinds.com:443"
api_token_value = "SOLARWINDS_OTEL_INGESTION_TOKEN"

# Optional tuning
log_level         = "INFO"
max_batch_records = 2000
max_batch_bytes   = 1500000
export_timeout    = "7s"
max_retries       = 3
workers           = 8
fn_memory_mb      = 512
fn_timeout_seconds = 60
fn_max_instances   = 10
