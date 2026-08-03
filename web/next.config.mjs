/** @type {import('next').NextConfig} */
const nextConfig = {
  // Required for the standalone Docker image (Dockerfile.web)
  output: 'standalone',
  async rewrites() {
    return [
      {
        source: '/api/admin/:path*',
        destination: 'http://nexus-admin:8081/admin/v1/:path*',
      },
      {
        // Gateway passthrough — used for endpoints that live on the inference
        // server (port 8880), e.g. GET /v1/providers/:name/models
        source: '/api/gateway/:path*',
        destination: 'http://nexus-gateway:8080/v1/:path*',
      },
    ]
  },
}

export default nextConfig
