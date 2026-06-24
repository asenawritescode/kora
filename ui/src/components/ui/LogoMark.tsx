export function LogoMark({ size = 24 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 32 32" fill="none" xmlns="http://www.w3.org/2000/svg">
      <rect width="32" height="32" rx="6" fill="#0A0A0A" />
      <path d="M8 22L16 10L24 22" stroke="#FF6B35" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}
