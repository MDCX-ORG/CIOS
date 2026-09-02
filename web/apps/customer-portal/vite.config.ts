import { reactRouter } from "@react-router/dev/vite";
import { defineConfig } from "vite";
import tsconfigPaths from "vite-tsconfig-paths";

export default defineConfig({
  plugins: [reactRouter(), tsconfigPaths()],
  // Default 3211 so customer-portal does not collide with ops-portal :3210.
  server: { port: Number(process.env.PORT ?? 3211) },
});
