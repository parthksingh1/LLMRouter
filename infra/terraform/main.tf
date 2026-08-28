/*
 * LLMRouter cloud infrastructure. REFERENCE ONLY.
 *
 * `make demo` does not use this. It provisions billable AWS resources and takes about twenty
 * minutes; the demo runs entirely on docker-compose. See infra/README.md.
 *
 * What this provisions:
 *   - an EKS cluster with a managed node group
 *   - ElastiCache Redis for the cache and the budget counters
 *   - an EC2-hosted ClickHouse (the choice is argued below)
 *   - IRSA so pods get AWS credentials without a static key anywhere
 *   - Secrets Manager for the provider API keys
 *
 * The ClickHouse decision, since it is the one a reviewer should push on: AWS has no managed
 * ClickHouse, and the alternatives are ClickHouse Cloud (excellent, but a second vendor and a
 * second bill) or self-hosting. A single EC2 instance with gp3 storage is the honest starting
 * point for an analytics workload of this size, and the module makes the upgrade path explicit
 * rather than pretending the problem does not exist. Argued in
 * docs/adr/0004-clickhouse-vs-postgres.md.
 */

terraform {
  required_version = ">= 1.6"

  required_providers {
    aws        = { source = "hashicorp/aws", version = "~> 5.80" }
    kubernetes = { source = "hashicorp/kubernetes", version = "~> 2.35" }
    random     = { source = "hashicorp/random", version = "~> 3.6" }
  }

  # Commented rather than omitted: local state is fine for a reference module and wrong for
  # anything shared, and saying so is more useful than a backend block nobody can use.
  #
  # backend "s3" {
  #   bucket         = "llmrouter-tfstate"
  #   key            = "llmrouter/terraform.tfstate"
  #   region         = "eu-west-1"
  #   dynamodb_table = "llmrouter-tflock"
  #   encrypt        = true
  # }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project     = "llmrouter"
      Environment = var.environment
      ManagedBy   = "terraform"
    }
  }
}

locals {
  name = "llmrouter-${var.environment}"

  # Two AZs, not three. The gateway is stateless and the stateful services are managed or
  # single-instance, so the third AZ buys availability this architecture cannot use while
  # tripling cross-AZ data transfer -- which for an LLM gateway moving large payloads is a real
  # line on the bill rather than a rounding error.
  azs = slice(data.aws_availability_zones.available.names, 0, 2)
}

data "aws_availability_zones" "available" {
  state = "available"
}

# ------------------------------------------------------------------------------------------
# Network
# ------------------------------------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = { Name = local.name }
}

resource "aws_subnet" "private" {
  count = length(local.azs)

  vpc_id            = aws_vpc.main.id
  cidr_block        = cidrsubnet(var.vpc_cidr, 4, count.index)
  availability_zone = local.azs[count.index]

  tags = {
    Name                              = "${local.name}-private-${count.index}"
    "kubernetes.io/role/internal-elb" = "1"
  }
}

resource "aws_subnet" "public" {
  count = length(local.azs)

  vpc_id                  = aws_vpc.main.id
  cidr_block              = cidrsubnet(var.vpc_cidr, 4, count.index + length(local.azs))
  availability_zone       = local.azs[count.index]
  map_public_ip_on_launch = true

  tags = {
    Name                     = "${local.name}-public-${count.index}"
    "kubernetes.io/role/elb" = "1"
  }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = local.name }
}

# One NAT gateway, not one per AZ. It is a single point of failure for egress and it is the
# right trade at this size: a NAT gateway is roughly $35/month plus data, and three of them for
# a workload that can tolerate an egress blip is money better spent elsewhere. Set
# var.nat_per_az when that stops being true.
resource "aws_eip" "nat" {
  count  = var.nat_per_az ? length(local.azs) : 1
  domain = "vpc"
  tags   = { Name = "${local.name}-nat-${count.index}" }
}

resource "aws_nat_gateway" "main" {
  count = var.nat_per_az ? length(local.azs) : 1

  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id
  depends_on    = [aws_internet_gateway.main]

  tags = { Name = "${local.name}-${count.index}" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }

  tags = { Name = "${local.name}-public" }
}

resource "aws_route_table" "private" {
  count  = length(local.azs)
  vpc_id = aws_vpc.main.id

  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.main[var.nat_per_az ? count.index : 0].id
  }

  tags = { Name = "${local.name}-private-${count.index}" }
}

resource "aws_route_table_association" "public" {
  count          = length(aws_subnet.public)
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table_association" "private" {
  count          = length(aws_subnet.private)
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private[count.index].id
}

# ------------------------------------------------------------------------------------------
# EKS
# ------------------------------------------------------------------------------------------

resource "aws_iam_role" "cluster" {
  name = "${local.name}-cluster"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "eks.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "cluster" {
  role       = aws_iam_role.cluster.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
}

resource "aws_eks_cluster" "main" {
  name     = local.name
  role_arn = aws_iam_role.cluster.arn
  version  = var.kubernetes_version

  vpc_config {
    subnet_ids              = concat(aws_subnet.private[*].id, aws_subnet.public[*].id)
    endpoint_private_access = true
    endpoint_public_access  = true
    public_access_cidrs     = var.api_allowed_cidrs
  }

  # Envelope encryption for Secrets. The gateway's provider API keys live in a Kubernetes
  # Secret, and without this they sit base64-encoded in etcd.
  encryption_config {
    provider { key_arn = aws_kms_key.eks.arn }
    resources = ["secrets"]
  }

  enabled_cluster_log_types = ["api", "audit", "authenticator"]

  depends_on = [aws_iam_role_policy_attachment.cluster]
}

resource "aws_kms_key" "eks" {
  description             = "${local.name} EKS secret envelope encryption"
  deletion_window_in_days = 7
  enable_key_rotation     = true
}

resource "aws_iam_role" "node" {
  name = "${local.name}-node"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "node" {
  for_each = toset([
    "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
    "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
    "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
  ])

  role       = aws_iam_role.node.name
  policy_arn = each.value
}

resource "aws_eks_node_group" "main" {
  cluster_name    = aws_eks_cluster.main.name
  node_group_name = "${local.name}-general"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = aws_subnet.private[*].id

  # Compute-optimised: the gateway is network-bound but the embedder sidecar runs ONNX
  # inference on CPU, and that is what sets the instance type.
  instance_types = var.node_instance_types
  capacity_type  = "ON_DEMAND"

  scaling_config {
    desired_size = var.node_desired_size
    min_size     = var.node_min_size
    max_size     = var.node_max_size
  }

  update_config {
    max_unavailable = 1
  }

  depends_on = [aws_iam_role_policy_attachment.node]

  lifecycle {
    # The HPA and cluster-autoscaler own the replica count after creation; Terraform reasserting
    # desired_size on every apply would fight them.
    ignore_changes = [scaling_config[0].desired_size]
  }
}

# IRSA. Pods assume an IAM role through the OIDC provider rather than carrying a static access
# key, which is the difference between a leaked pod spec being embarrassing and being an
# incident.
data "tls_certificate" "oidc" {
  url = aws_eks_cluster.main.identity[0].oidc[0].issuer
}

resource "aws_iam_openid_connect_provider" "eks" {
  url             = aws_eks_cluster.main.identity[0].oidc[0].issuer
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = [data.tls_certificate.oidc.certificates[0].sha1_fingerprint]
}

# ------------------------------------------------------------------------------------------
# Redis
# ------------------------------------------------------------------------------------------

resource "aws_elasticache_subnet_group" "main" {
  name       = local.name
  subnet_ids = aws_subnet.private[*].id
}

resource "aws_security_group" "redis" {
  name   = "${local.name}-redis"
  vpc_id = aws_vpc.main.id

  ingress {
    from_port       = 6379
    to_port         = 6379
    protocol        = "tcp"
    security_groups = [aws_eks_cluster.main.vpc_config[0].cluster_security_group_id]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_elasticache_replication_group" "main" {
  replication_group_id = local.name
  description          = "LLMRouter semantic cache and budget counters"

  engine         = "redis"
  engine_version = "7.1"
  node_type      = var.redis_node_type
  port           = 6379

  # Two nodes with automatic failover. The budget counters must survive a node loss: losing
  # them mid-day resets every tenant's spend to zero, which is a billing incident rather than
  # a cache miss.
  num_cache_clusters         = 2
  automatic_failover_enabled = true
  multi_az_enabled           = true

  subnet_group_name  = aws_elasticache_subnet_group.main.name
  security_group_ids = [aws_security_group.redis.id]

  at_rest_encryption_enabled = true
  transit_encryption_enabled = true
  auth_token                 = random_password.redis.result

  # Cache entries are recomputable; the budget counters are not, but they are also
  # short-lived. Daily snapshots are the cheap middle ground.
  snapshot_retention_limit = 1
  maintenance_window       = "sun:05:00-sun:07:00"

  # allkeys-lru: under memory pressure the semantic cache should shed entries rather than
  # start refusing writes, which would take the budget counters down with it.
  parameter_group_name = aws_elasticache_parameter_group.main.name
}

resource "aws_elasticache_parameter_group" "main" {
  name   = local.name
  family = "redis7"

  parameter {
    name  = "maxmemory-policy"
    value = "allkeys-lru"
  }
}

resource "random_password" "redis" {
  length  = 32
  special = false
}

# ------------------------------------------------------------------------------------------
# Secrets
# ------------------------------------------------------------------------------------------

resource "aws_secretsmanager_secret" "provider_keys" {
  name                    = "${local.name}/provider-keys"
  description             = "Upstream LLM provider API keys"
  recovery_window_in_days = 7
}

# The secret VALUE is deliberately not managed here. Putting it in Terraform puts it in state,
# and state is a file that gets copied. The resource creates the container; a human or a
# pipeline with narrower permissions fills it.
resource "aws_secretsmanager_secret_version" "provider_keys" {
  secret_id = aws_secretsmanager_secret.provider_keys.id
  secret_string = jsonencode({
    OPENAI_API_KEY    = "REPLACE_ME"
    ANTHROPIC_API_KEY = "REPLACE_ME"
    GOOGLE_API_KEY    = "REPLACE_ME"
    MISTRAL_API_KEY   = "REPLACE_ME"
    TOGETHER_API_KEY  = "REPLACE_ME"
  })

  lifecycle {
    ignore_changes = [secret_string]
  }
}
