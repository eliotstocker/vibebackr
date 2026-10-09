# Provision a Neon serverless PostgreSQL project
# Automatically provisions a default branch, database, role, and pooled compute endpoint
resource "neon_project" "vibebackr" {
  name                      = var.app_name
  region_id                 = var.neon_region_id
  pg_version                = 16
  org_id                    = var.neon_org_id
  history_retention_seconds = 21600
}
