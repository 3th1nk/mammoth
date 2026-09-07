// Multi-architecture image build matrix (amd64 + arm64) for both delivery
// images (docs/02-architecture.md §5.3).
group "default" {
  targets = ["mammoth", "mammoth-builder"]
}

target "common" {
  platforms = ["linux/amd64", "linux/arm64"]
}

target "mammoth" {
  inherits = ["common"]
  dockerfile = "Dockerfile"
  context = "."
  tags = ["mammoth:dev"]
}

target "mammoth-builder" {
  inherits = ["common"]
  dockerfile = "Dockerfile.builder"
  context = "."
  tags = ["mammoth-builder:dev"]
}
