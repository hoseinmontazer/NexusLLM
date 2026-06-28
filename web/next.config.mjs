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
    ]
  },
}

export default nextConfig
