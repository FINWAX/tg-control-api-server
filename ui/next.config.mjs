/** @type {import('next').NextConfig} */
const nextConfig = {
  // Static export: `next build` emits a fully static site into ./out, which
  // Caddy serves. All API calls go to the same origin (/v1/*), reverse-proxied
  // by Caddy to the gateway — so no CORS and no runtime Node server.
  output: 'export',
  reactStrictMode: true,
};

export default nextConfig;
