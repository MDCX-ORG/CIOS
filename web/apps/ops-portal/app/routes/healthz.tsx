// Resource route (unauthenticated portal liveness). No default export
// — RR7 would otherwise render a UI component around the loader response.
export function loader() {
  return Response.json({ status: "ok" });
}
