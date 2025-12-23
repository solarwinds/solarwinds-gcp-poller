# GCP Log Forwarder

This repository deploys a **Cloud Logging → Pub/Sub → Cloud Functions Gen2** pipeline on Google Cloud using Terraform.

It captures selected **GCP  Logs** (for example, Compute Engine VM lifecycle events) and forwards them to an external **OTLP-compatible endpoint** using a Go-based Cloud Function.

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

## Repository structure

```
.
├── main.tf
├── variables.tf
├── outputs.tf
├── terraform.tfvars
├── function_src/        # Go function source code
│   └── main.go
└── README.md
```

---

## GCP authentication (local development)

Terraform uses **Application Default Credentials (ADC)**.

### Step 1: Install Google Cloud SDK

https://cloud.google.com/sdk/docs/install

### Step 2: Authenticate

```bash
gcloud auth application-default login
```

This opens a browser and stores credentials locally.

### Step 3: Set the active project

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

topic_name = "vm-events"
sink_name  = "vm-events-sink"

function_name   = "ForwardLogs"
otlp_endpoint   = "example.collector.com:443"
api_token_value = "REDACTED"
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
