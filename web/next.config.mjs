/** @type {import('next').NextConfig} */
const nextConfig = {
  // Required for the standalone Docker image (Dockerfile.web)
  output: 'standalone',
  async rewrites() {
    return [
      {
        // Admin API proxy — all UI calls go to the admin server.
        // The admin server is the only backend the UI talks to directly.
        // Live model data (GET /providers/:id/live-models) is proxied
        // server-side by the admin handler using stored credentials,
        // so no gateway proxy rewrite is needed here.
        source: '/api/admin/:path*',
        destination: 'http://nexus-admin:8081/admin/v1/:path*',
      },
      {
        source: '/portal/v1/:path*',
        destination: 'http://nexus-admin:8081/portal/v1/:path*',
      },
      {
        source: '/admin/v1/:path*',
        destination: 'http://nexus-admin:8081/admin/v1/:path*',
      },
    ]
  },
}

export default nextConfig
