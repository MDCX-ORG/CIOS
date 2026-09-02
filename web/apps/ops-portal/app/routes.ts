import { type RouteConfig, index, route } from "@react-router/dev/routes";

export default [
  index("routes/_index.tsx"),
  route("login", "routes/login.tsx"),
  route("logout", "routes/logout.tsx"),
  route("healthz", "routes/healthz.tsx"),
  route("noc", "routes/noc.tsx"),
  route("noc/cause/:crn", "routes/noc.cause.$crn.tsx"),
  route("assets", "routes/assets.tsx"),
  route("alarms", "routes/alarms.tsx"),
  route("tickets", "routes/tickets.tsx"),
  route("maintenance", "routes/maintenance.tsx"),
  route("spares", "routes/spares.tsx"),
  route("inspections", "routes/inspections.tsx"),
  route("runbooks", "routes/runbooks.tsx"),
  route("reports", "routes/reports.tsx"),
  route("usage", "routes/usage.tsx"),
  // L109 Platform Admin (ops-portal /admin/*)
  route("admin", "routes/admin.tsx"),
  route("admin/onboard", "routes/admin.onboard.tsx"),
  route("admin/sites", "routes/admin.sites.tsx"),
  route("admin/tenants", "routes/admin.tenants.tsx"),
  route("admin/users", "routes/admin.users.tsx"),
  route("admin/models", "routes/admin.models.tsx"),
  route("admin/models/:type/:model", "routes/admin.models.$type.$model.tsx"),
  route("admin/draw", "routes/admin.draw.tsx"),
  route("api/stream/:site", "routes/api.stream.$site.tsx"),
] satisfies RouteConfig;
