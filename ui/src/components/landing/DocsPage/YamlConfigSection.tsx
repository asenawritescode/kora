import { Copy } from 'lucide-react'

const YAML_EXAMPLE = `version: "1.0"
name: my-frontend-service
type: web

deployment:
  image: registry.kora.io/library/nginx:alpine
  replicas: 3
  regions:
    - us-east-1
    - eu-west-1

resources:
  cpu: "0.5"
  memory: "512Mi"

env:
  - NODE_ENV: production
  - API_ENDPOINT: https://api.internal.local`

export function YamlConfigSection() {
  return (
    <section className="mb-[120px]">
      <h2 className="text-[30px] leading-[38px] font-semibold text-black mb-8 border-b border-outline-variant pb-2">
        YAML Configuration
      </h2>
      <p className="mb-4 text-[#5d5f5f]">
        Workloads are defined declaratively using standard YAML syntax. Below is an example of a basic web service deployment configuration.
      </p>
      <div className="bg-[#0A0A0A] rounded-sm border border-outline-variant overflow-hidden">
        <div className="flex items-center justify-between px-4 py-2 bg-[#1A1A1A] border-b border-outline-variant/30">
          <span className="text-sm text-[#5d5f5f]" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>kora-service.yaml</span>
          <button className="text-[#5d5f5f] hover:text-white transition-colors">
            <Copy className="h-3.5 w-3.5" />
          </button>
        </div>
        <pre className="p-4 text-sm text-[#E5E5E5] overflow-x-auto" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
          <code>
            {YAML_EXAMPLE.split('\n').map((line, i) => {
              const keyMatch = line.match(/^(\s*)([a-zA-Z_-]+):/)
              if (keyMatch) {
                const indent = keyMatch[1]
                const key = keyMatch[2]
                const rest = line.slice(keyMatch[0].length)
                return (
                  <span key={i}>
                    {indent}<span className="text-[#FF6B35]">{key}:</span>{rest}{'\n'}
                  </span>
                )
              }
              return <span key={i}>{line}{'\n'}</span>
            })}
          </code>
        </pre>
      </div>
    </section>
  )
}
