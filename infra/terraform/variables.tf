variable "region" {
  description = "AWS region"
  type        = string
  default     = "eu-west-1"
}

variable "environment" {
  description = "Environment name, used in every resource name"
  type        = string
  default     = "dev"
}

variable "vpc_cidr" {
  description = "VPC CIDR block"
  type        = string
  default     = "10.42.0.0/16"
}

variable "kubernetes_version" {
  description = "EKS control plane version"
  type        = string
  default     = "1.31"
}

# Defaults to 0.0.0.0/0 so the module applies cleanly as a reference. Narrow it before this goes
# anywhere real: a public API endpoint is the front door to the cluster.
variable "api_allowed_cidrs" {
  description = "CIDRs allowed to reach the EKS public API endpoint"
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

# Compute-optimised because the embedder sidecar runs ONNX inference on CPU. The gateway itself
# is network-bound and would be happy on almost anything.
variable "node_instance_types" {
  description = "Node instance types"
  type        = list(string)
  default     = ["c6i.xlarge"]
}

variable "node_desired_size" {
  type    = number
  default = 3
}

variable "node_min_size" {
  type    = number
  default = 2
}

variable "node_max_size" {
  type    = number
  default = 10
}

variable "redis_node_type" {
  description = "ElastiCache node type"
  type        = string
  default     = "cache.t4g.medium"
}

# False by default. A NAT gateway is roughly $35/month plus data transfer, and this architecture
# tolerates an egress blip; turn it on when it stops tolerating one.
variable "nat_per_az" {
  description = "One NAT gateway per AZ instead of one shared"
  type        = bool
  default     = false
}

variable "clickhouse_instance_type" {
  description = "EC2 instance type for ClickHouse"
  type        = string
  default     = "m6i.xlarge"
}

variable "clickhouse_volume_gb" {
  description = "gp3 volume size for ClickHouse data"
  type        = number
  default     = 500
}
