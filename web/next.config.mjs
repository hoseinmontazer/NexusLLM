/** @type {import('next').NextConfig} */
const nextConfig = {
  async rewrites() {
    return [
      {
        source: '/api/admin/:path*',
        destination: 'http://localhost:8081/admin/v1/:path*',
      },
    ]
  },
}

export default nextConfig
