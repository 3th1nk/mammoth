// Multi-architecture image build matrix (amd64 + arm64) for both delivery
// images (docs/02-architecture.md §5.3).
group "default" {
  targets = ["mammoth", "mammoth-api"]
}

target "common" {
  platforms = ["linux/amd64", "linux/arm64"]
}

target "mammoth" {
  inherits = ["common"]
  dockerfile = "Dockerfile.api"
  context = "."
  tags = ["mammoth:dev"]
}

target "mammoth-api" {
  inherits = ["common"]
  dockerfile = "Dockerfile.api"
  context = "."
  tags = ["mammoth-api:dev"]
}
