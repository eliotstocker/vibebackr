# Serverless Terraform Deployment: AWS Lambda + Neon + Custom Domain

This Terraform configuration deploys **VibeBackr** as a serverless application on **AWS Lambda** backed by **Neon Serverless PostgreSQL** (with native `pgvector`), fronted by **Amazon CloudFront** and **AWS Route 53** on your custom domain **`vibebac.kr`**.

---

## 🏗️ Architecture

```text
                           ┌───────────────────────────┐
                           │   AI Agents / REST Clients │
                           └─────────────┬─────────────┘
                                         │ (HTTPS: https://vibebac.kr)
                                         ▼
                 ┌────────────────────────────────────────────────┐
                 │       Route 53 DNS (vibebac.kr / *.vibebac.kr) │
                 └───────────────────────┬────────────────────────┘
                                         │
                                         ▼
                 ┌────────────────────────────────────────────────┐
                 │       CloudFront CDN + ACM SSL (us-east-1)     │
                 │   - Preserves streaming & SSE for /mcp         │
                 │   - Managed-AllViewerExceptHostHeader          │
                 │   - Managed-CachingDisabled                    │
                 └───────────────────────┬────────────────────────┘
                                         │
                                         ▼
                 ┌────────────────────────────────────────────────┐
                 │       AWS Lambda Function URL (Streaming)      │
                 │   - Authorization: None (VibeBackr handles Auth)│
                 │   - Invoke Mode: RESPONSE_STREAM (for SSE)     │
                 └───────────────────────┬────────────────────────┘
                                         │
                                         ▼
                 ┌────────────────────────────────────────────────┐
                 │        AWS Lambda (provided.al2023 / ARM64)    │
                 │                                                │
                 │  ┌──────────────────────────────────────────┐  │
                 │  │ AWS Lambda Web Adapter (/opt/bootstrap)  │  │
                 │  └────────────────────┬─────────────────────┘  │
                 │                       │ (HTTP localhost:8080)  │
                 │  ┌────────────────────▼─────────────────────┐  │
                 │  │ VibeBackr Go Binary (cmd/vibebackr serve)│  │
                 │  └────────────────────┬─────────────────────┘  │
                 └───────────────────────┼────────────────────────┘
                                         │
                                         ▼
                 ┌────────────────────────────────────────────────┐
                 │       Neon Serverless PostgreSQL (pg16)        │
                 │   - Connection Pooler (-pooler)                │
                 │   - Auto-migrated schema on cold start         │
                 │   - Native pgvector enabled                    │
                 └────────────────────────────────────────────────┘
```

### Highlights:
* **Custom Domain & SSL:** Automated Route 53 hosted zone management and AWS Certificate Manager (ACM) DNS validation for `vibebac.kr` and `*.vibebac.kr`.
* **Zero Go Code Changes:** Uses the official **AWS Lambda Web Adapter** (`LambdaAdapterLayerArm64`) layer to run the standard Go `http.Server` directly on AWS Lambda without Lambda-specific HTTP proxy wrappers.
* **Streamable MCP Support:** The Function URL, CloudFront, and Lambda Web Adapter are configured for `RESPONSE_STREAM`, enabling real-time Server-Sent Events (SSE) on `/mcp`.
* **Host Header Protection:** CloudFront uses `Managed-AllViewerExceptHostHeader` so Lambda Function URLs receive their internal hostname while passing client auth headers through untouched.
* **Zero-CGO ARM64:** Compiles to a static Linux ARM64 binary (`provided.al2023`) for high performance and low execution cost on Graviton.
* **Automatic Migrations & pgvector:** On cold start, VibeBackr automatically executes PostgreSQL migrations and provisions `pgvector` tables on Neon.
* **Connection Pooling:** Connects through Neon's connection pooler endpoint to handle rapid Lambda concurrency spikes.

---

## 🚀 Quick Start

### 1. Prerequisites
* [Terraform](https://www.terraform.io/downloads.html) >= 1.5.0
* [Go](https://golang.org/dl/) >= 1.22
* [AWS CLI](https://aws.amazon.com/cli/) configured (`aws configure`)
* A [Neon](https://neon.tech) account and API Key (`export NEON_API_KEY="your-token"`)
* Ownership of `vibebac.kr`

### 2. Configure Variables
Copy the example variables file:
```bash
cp terraform.tfvars.example terraform.tfvars
```
Edit `terraform.tfvars`:
```hcl
aws_region          = "us-east-1"
domain_name         = "vibebac.kr"
create_route53_zone = true # Set false if zone already exists in your AWS account

neon_region_id      = "aws-us-east-1" # keep in same region as Lambda for minimum latency
neon_api_key        = "your-neon-api-key"
app_name            = "vibe-backr"
```

### 3. Initialize & Deploy
```bash
terraform init
terraform apply
```

During `apply`, Terraform will:
1. Compile the pure-Go binary targeting Linux ARM64 into `terraform/dist/vibebackr`.
2. Zip the executable into `terraform/dist/lambda.zip`.
3. Provision the Neon project and retrieve the pooled connection URI.
4. Create the Route 53 hosted zone and request an ACM SSL certificate with DNS validation.
5. Create the IAM role, CloudWatch log group, and Lambda function with the Web Adapter layer.
6. Create the streaming Function URL and CloudFront distribution with alias records.

### 4. Delegate Nameservers (If registered outside AWS Route 53)
If you purchased `vibebac.kr` through an external registrar (e.g., Namecheap, Porkbun, GoDaddy), copy the 4 nameservers from Terraform's output and update the NS records at your registrar:
```bash
terraform output route53_nameservers
```

### 5. Outputs
After deployment completes, Terraform outputs the endpoints:
```text
Outputs:

custom_domain_url              = "https://vibebac.kr"
custom_mcp_endpoint            = "https://vibebac.kr/mcp"
route53_nameservers            = [ "ns-xxx.awsdns-xx.com", ... ]
lambda_direct_url              = "https://<unique-id>.lambda-url.us-east-1.on.aws/"
cloudfront_distribution_domain = "d111111abcdef8.cloudfront.net"
neon_project_id                = "project-name-123456"
```

---

## 🧪 Verifying the Deployment

### Test Custom Domain REST API
```bash
curl https://vibebac.kr/api/health
```

### Test Streamable MCP Endpoint
Connect an AI agent or test Server-Sent Events via curl:
```bash
curl -N -H "Accept: text/event-stream" https://vibebac.kr/mcp
```
