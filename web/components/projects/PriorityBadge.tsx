'use client'

interface PriorityBadgeProps {
  weight: number
  label?: string
  showWeight?: boolean
  size?: 'sm' | 'md'
}

function weightToStyle(w: number): string {
  if (w >= 900) return 'bg-red-100 text-red-800 border border-red-300'
  if (w >= 700) return 'bg-orange-100 text-orange-800 border border-orange-300'
  if (w >= 500) return 'bg-blue-100 text-blue-800 border border-blue-300'
  if (w >= 300) return 'bg-gray-100 text-gray-600 border border-gray-300'
  return 'bg-gray-50 text-gray-400 border border-gray-200'
}

export function weightLabel(w: number): string {
  if (w >= 950) return 'Emergency'
  if (w >= 900) return 'Prod Critical'
  if (w >= 800) return 'Revenue Critical'
  if (w >= 700) return 'Core Internal'
  if (w >= 500) return 'Standard'
  if (w >= 300) return 'Batch'
  if (w >= 100) return 'Development'
  if (w >= 50)  return 'Playground'
  return 'Best Effort'
}

/** Visual priority bar: filled proportion = weight / 1000 */
export function PriorityBar({ weight, className = '' }: { weight: number; className?: string }) {
  const pct = Math.min(100, Math.max(0, (weight / 1000) * 100))
  const color = weight >= 900 ? 'bg-red-500' : weight >= 700 ? 'bg-orange-400' : weight >= 500 ? 'bg-blue-500' : weight >= 300 ? 'bg-gray-400' : 'bg-gray-200'
  return (
    <div className={`h-1.5 w-full rounded-full bg-gray-100 overflow-hidden ${className}`}>
      <div className={`h-full rounded-full ${color} transition-all`} style={{ width: `${pct}%` }} />
    </div>
  )
}

export function PriorityBadge({ weight, label, showWeight = false, size = 'sm' }: PriorityBadgeProps) {
  const displayLabel = label ?? weightLabel(weight)
  const style = weightToStyle(weight)
  const px = size === 'md' ? 'px-2.5 py-1 text-sm' : 'px-2 py-0.5 text-xs'
  return (
    <span className={`inline-flex items-center gap-1 rounded-full font-semibold ${px} ${style}`}>
      {displayLabel}
      {showWeight && <span className="opacity-60">·{weight}</span>}
    </span>
  )
}

/** Effective priority breakdown tooltip-style card */
export function EffectivePriorityCard({
  baseWeight, waitingBonus, reservationBonus, resourcePenalty, effective
}: {
  baseWeight: number
  waitingBonus: number
  reservationBonus: number
  resourcePenalty: number
  effective: number
}) {
  return (
    <div className="text-xs space-y-1 font-mono">
      <div className="flex justify-between gap-4">
        <span className="text-muted-foreground">Base Priority</span>
        <span className="font-semibold">{baseWeight}</span>
      </div>
      {waitingBonus > 0 && (
        <div className="flex justify-between gap-4 text-green-700">
          <span>Waiting Bonus</span>
          <span>+{waitingBonus}</span>
        </div>
      )}
      {reservationBonus > 0 && (
        <div className="flex justify-between gap-4 text-blue-700">
          <span>Reservation Bonus</span>
          <span>+{reservationBonus}</span>
        </div>
      )}
      {resourcePenalty > 0 && (
        <div className="flex justify-between gap-4 text-red-700">
          <span>Resource Penalty</span>
          <span>−{resourcePenalty}</span>
        </div>
      )}
      <div className="border-t pt-1 flex justify-between gap-4">
        <span className="font-semibold text-foreground">Effective Priority</span>
        <span className="font-bold text-base text-foreground">{effective}</span>
      </div>
      <PriorityBar weight={effective} className="mt-1" />
    </div>
  )
}
