/** Identify transient request-detail state by the tenant, actor and request. */
export function requestDetailScope(tenant: string, actor: string, requestID: string): string {
  return JSON.stringify([tenant, actor, requestID])
}
