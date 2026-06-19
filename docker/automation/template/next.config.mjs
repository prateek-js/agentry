// Automations run as a long-lived Node server (the scheduler lives in
// the process). Standalone Node output — never static export or edge,
// or the in-process cron silently never fires.
const DB_DRIVERS = ['pg', 'pg-native', 'mysql2', 'mongodb', 'ioredis']

/** @type {import('next').NextConfig} */
export default {
  output: 'standalone',
  // @agentry/automation is installed as a linked package that may resolve
  // outside the project dir; let webpack reach (and transpile) it.
  experimental: {
    instrumentationHook: true,
    externalDir: true,
    // Keep the DB drivers out of the RSC/route bundles + trace them into
    // the standalone output. They're dynamic-imported by the store, so
    // only the one matching the bound DB ever actually loads at runtime.
    serverComponentsExternalPackages: DB_DRIVERS,
  },
  webpack: (config, { isServer }) => {
    if (isServer) {
      // The instrumentation bundle doesn't honor serverComponentsExternalPackages,
      // so externalize the drivers here too — otherwise webpack tries to bundle
      // native `pg`/`mysql2` and fails on their Node built-in imports.
      const externalize = ({ request }, cb) => {
        // node: builtins (the automation lib imports `node:crypto`) — keep
        // them as runtime requires; webpack's bundler can't read the scheme.
        if (request.startsWith('node:')) return cb(null, 'commonjs ' + request)
        if (DB_DRIVERS.some((d) => request === d || request.startsWith(d + '/')))
          return cb(null, 'commonjs ' + request)
        return cb()
      }
      config.externals = Array.isArray(config.externals)
        ? [externalize, ...config.externals]
        : [externalize, config.externals].filter(Boolean)
    }
    return config
  },
}
