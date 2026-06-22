import type { ProjectPriority } from '@/lib/api'

const colours: Record<ProjectPriority, string> = {
  CRITICAL:    'bg-red-100 text-red-800 border border-red-300',
  HIGH:        'bg-orange-100 text-orange-800 border border-orange-300',
  NORMAL:      'bg-blue-100 text-blue-800 border border-blue-300',
  LOW:         'bg-gray-100 text-gray-600 border border-gray-300',
  BEST_EFFORT: 'bg-gray-50 text-gray-400 border border-gray-200',
}

export function PriorityBadge({ priority }: { priority: ProjectPriority }) {
  return (
    <span className={`px-2 py-0.5 rounded-full text-xs font-semibold ${colours[priority] ?? colours.NORMAL}`}>
      {priority}
    </span>
  )
}
