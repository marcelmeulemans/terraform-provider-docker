resource "docker_image" "test" {
  name         = "alpine:3.15.0"
  force_remove = true
}
