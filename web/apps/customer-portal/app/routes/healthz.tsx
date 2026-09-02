/** Unauthenticated liveness for the customer-portal process. */
export function loader() {
  return Response.json({ status: "ok" });
}
