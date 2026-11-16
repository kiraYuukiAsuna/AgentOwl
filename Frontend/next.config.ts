import type { NextConfig } from "next";
const config: NextConfig = {
  output: "standalone",
  redirects() {
    return [{ source: "/admission", destination: "/settings/policies", permanent: false }];
  },
  webpack(config) {
    config.resolve.extensionAlias = {
      ...config.resolve.extensionAlias,
      ".js": [".ts", ".tsx", ".js"],
    };
    return config;
  },
  async rewrites() {
    const api = process.env.API_INTERNAL_URL ?? "http://127.0.0.1:8080";
    return [
      { source: "/api/:path*", destination: `${api}/api/:path*` },
      { source: "/auth/:path*", destination: `${api}/auth/:path*` },
    ];
  },
};
export default config;
