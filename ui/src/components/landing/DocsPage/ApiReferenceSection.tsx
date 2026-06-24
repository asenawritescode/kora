export function ApiReferenceSection() {
  return (
    <section>
      <h2 className="text-[30px] leading-[38px] font-semibold text-black mb-8 border-b border-outline-variant pb-2">
        API Reference
      </h2>
      <p className="mb-4 text-[#5d5f5f]">
        Interact with your infrastructure programmatically via our REST API. All endpoints are secured via Bearer token authentication.
      </p>
      <div className="border border-outline-variant rounded-sm overflow-hidden mb-4">
        {/* Method bar */}
        <div className="flex items-center gap-3 p-3 bg-[#f1edec] border-b border-outline-variant">
          <span className="px-2 py-0.5 bg-black text-white rounded-sm text-[10px] uppercase font-bold" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>GET</span>
          <code className="text-sm text-black" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>/v1/clusters</code>
        </div>
        {/* Parameters */}
        <div className="p-4 bg-[#FAFAFA]">
          <h4 className="font-bold text-sm mb-2">Parameters</h4>
          <div className="border-t border-outline-variant pt-2 space-y-3">
            <div className="flex items-start gap-4">
              <code className="text-sm w-24 shrink-0" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>region</code>
              <div>
                <span className="text-xs text-[#5d5f5f] mb-1 block">string (optional)</span>
                <p className="text-sm text-[#5d5f5f]">Filter clusters by specific geographic region.</p>
              </div>
            </div>
            <div className="flex items-start gap-4">
              <code className="text-sm w-24 shrink-0" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>status</code>
              <div>
                <span className="text-xs text-[#5d5f5f] mb-1 block">string (optional)</span>
                <p className="text-sm text-[#5d5f5f]">Filter by current cluster status (active, provisioning, failed).</p>
              </div>
            </div>
          </div>
        </div>
      </div>
    </section>
  )
}
