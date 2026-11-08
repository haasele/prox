class Prox < Formula
  desc "One global HTTPS/TCP gateway for Docker containers and local dev processes"
  homepage "https://github.com/haasele/prox"
  url "https://github.com/haasele/prox/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "MIT"
  head "https://github.com/haasele/prox.git", branch: "main"

  depends_on "go" => :build

  def install
    ENV["CGO_ENABLED"] = "0"
    ldflags = "-s -w -X github.com/haasele/prox/internal/version.Version=#{version}"
    system "go", "build", "-trimpath", "-ldflags", ldflags, "-o", bin/"prox", "./cmd/prox"
    bin.install_symlink "prox" => "gw"
    bin.install_symlink "prox" => "proxd"

    generate_completions_from_executable(bin/"prox", "completions")
  end

  test do
    assert_match "prox", shell_output("#{bin}/prox version")
  end
end
