# GCP Log Forwarder

This repository deploys a **Cloud Logging → Pub/Sub → Cloud Functions Gen2** pipeline on Google Cloud using Terraform.

It captures selected **GCP  Logs** (for example, Compute Engine VM lifecycle events) and forwards them to Solarwinds **OTLP-compatible endpoint** using a Go-based Cloud Function.

---

## What this deploys

- Required Google Cloud APIs
- Cloud Logging **sink** with configurable filter
- Pub/Sub **topic** for log delivery
- Cloud Functions **Gen2 (Go)** function triggered via Eventarc
- IAM bindings required for Cloud Build, Eventarc, Pub/Sub, and Cloud Run

---

## Prerequisites

- Terraform **>= 1.6.0**
- Google Cloud SDK (`gcloud`)
- A GCP project with billing enabled
- Permissions to create IAM bindings, Pub/Sub topics, Logging sinks, and Cloud Functions

---

## GCP authentication (local development)

Terraform uses **Application Default Credentials (ADC)**.

### Step 1: Git clone the Solarwinds gcp-poller public repo, and traverse to the gcp-log-forwarding

https://github.com/solarwinds/solarwinds-gcp-poller


### Step 2: Install Google Cloud SDK

https://cloud.google.com/sdk/docs/install

### Step 3: Authenticate

```bash
gcloud auth application-default login
```

This opens a browser and stores credentials locally.

### Step 4: Set the active project

```bash
gcloud config set project YOUR_PROJECT_ID
```

Terraform will automatically use these credentials.
No service account keys are required.

---

## Configuration

Edit `terraform.tfvars`:

```hcl
project_id = "my-gcp-project"
region     = "us-central1"

topic_name = "solarwinds-gcp-events"
sink_name  = "solarwinds-gcp-events-sink"

function_name   = "ForwardLogs"
otlp_endpoint   = "otel.collector.na-01.solarwinds.com:443"
api_token_value = "SOLARWINDS_OTEL_INGESTION_TOKEN"
```


---

## Deploy

```bash
terraform init
terraform plan
terraform apply
```

---

## Outputs

After deployment, Terraform prints:

- **Pub/Sub topic ID** receiving log entries
- **Logging sink writer identity**
- **Cloud Run service URI** backing the Cloud Function

---

## Notes

- The Cloud Function uses **internal-only ingress** and is invoked via Eventarc.
- Log volume depends on your logging filter – start narrow.
- Updating `terraform.tfvars` will redeploy the function.

---

## Cleanup

```bash
terraform destroy
```
