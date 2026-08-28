output "cluster_name" {
  description = "EKS cluster name"
  value       = aws_eks_cluster.main.name
}

output "cluster_endpoint" {
  description = "EKS API endpoint"
  value       = aws_eks_cluster.main.endpoint
}

output "oidc_provider_arn" {
  description = "IRSA OIDC provider ARN, for pod IAM roles"
  value       = aws_iam_openid_connect_provider.eks.arn
}

output "redis_endpoint" {
  description = "Redis primary endpoint"
  value       = aws_elasticache_replication_group.main.primary_endpoint_address
}

output "redis_auth_token" {
  description = "Redis AUTH token"
  value       = random_password.redis.result
  sensitive   = true
}

output "provider_keys_secret_arn" {
  description = "Secrets Manager ARN holding the provider API keys. Populate it separately."
  value       = aws_secretsmanager_secret.provider_keys.arn
}

output "kubeconfig_command" {
  description = "Command to configure kubectl"
  value       = "aws eks update-kubeconfig --region ${var.region} --name ${aws_eks_cluster.main.name}"
}
