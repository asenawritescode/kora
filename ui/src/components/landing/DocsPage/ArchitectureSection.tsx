export function ArchitectureSection() {
  return (
    <section className="mb-[120px]">
      <h2 className="text-[30px] leading-[38px] font-semibold text-black mb-8 border-b border-outline-variant pb-2">
        Architecture
      </h2>
      <p className="mb-4 text-[#5d5f5f]">
        Kora operates on a decentralized control plane model, ensuring high availability and robust partition tolerance across global regions.
      </p>

      {/* Diagram placeholder */}
      <div className="relative w-full h-64 bg-[#f1edec] border border-outline-variant rounded-sm mb-4 overflow-hidden flex items-center justify-center">
        <div className="absolute inset-0 bg-gradient-to-br from-gray-100 via-gray-50 to-white opacity-80" />
        <div className="relative text-center">
          <div className="flex items-center justify-center gap-8">
            <div className="w-20 h-20 rounded-full border-2 border-black flex items-center justify-center">
              <span className="text-xs font-bold">Control</span>
            </div>
            <div className="flex gap-1 text-2xl text-[#5d5f5f]">•••</div>
            <div className="w-16 h-16 rounded-full border-2 border-[#c4c7c7] flex items-center justify-center">
              <span className="text-[10px]">Worker</span>
            </div>
            <div className="w-16 h-16 rounded-full border-2 border-[#c4c7c7] flex items-center justify-center">
              <span className="text-[10px]">Worker</span>
            </div>
            <div className="flex gap-1 text-2xl text-[#5d5f5f]">•••</div>
            <div className="w-16 h-16 rounded-sm border-2 border-dashed border-[#c4c7c7] flex items-center justify-center">
              <span className="text-[10px]">Edge</span>
            </div>
          </div>
        </div>
        <div className="absolute inset-0 border border-outline-variant/50 pointer-events-none" />
      </div>

      <div className="grid grid-cols-1 md:grid-cols-3 gap-4 mt-8">
        <div className="border-l-2 border-black pl-4">
          <h4 className="font-bold text-black mb-1 text-sm">Control Plane</h4>
          <p className="text-[#5d5f5f] text-sm">Manages global state, routing, and policy enforcement across all active nodes.</p>
        </div>
        <div className="border-l-2 border-outline-variant pl-4">
          <h4 className="font-bold text-black mb-1 text-sm">Worker Nodes</h4>
          <p className="text-[#5d5f5f] text-sm">Executes localized workloads close to the end-user for minimal latency.</p>
        </div>
        <div className="border-l-2 border-outline-variant pl-4">
          <h4 className="font-bold text-black mb-1 text-sm">Edge Gateway</h4>
          <p className="text-[#5d5f5f] text-sm">Handles ingress traffic, TLS termination, and preliminary DDoS mitigation.</p>
        </div>
      </div>
    </section>
  )
}
