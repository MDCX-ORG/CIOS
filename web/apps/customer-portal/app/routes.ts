import { type RouteConfig, index, route } from "@react-router/dev/routes";

export default [
  index("routes/_index.tsx"),
  route("login", "routes/login.tsx"),
  route("logout", "routes/logout.tsx"),
  route("status", "routes/status.tsx"),
  route("sla", "routes/sla.tsx"),
  route("usage", "routes/usage.tsx"),
  route("healthz", "routes/healthz.tsx"),
] satisfies RouteConfig;
