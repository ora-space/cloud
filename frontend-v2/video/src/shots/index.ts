import type { Shot } from '../record/shot.ts'
import { oraOverview } from './ora-overview.ts'
import { newcomerCdase } from './newcomer-cdase.ts'
import { qualityScan } from './quality-scan.ts'
import { dashboardUsage } from './dashboard-usage.ts'
import { teamHuman } from './team-human.ts'
import { teamReview } from './team-review.ts'
import { teamSplit } from './team-split.ts'

/** Every shot the recorder knows, by name, in the order the film plays them. */
export const shots: Record<string, Shot> = {
  [oraOverview.name]: oraOverview,
  [newcomerCdase.name]: newcomerCdase,
  [teamSplit.name]: teamSplit,
  [teamHuman.name]: teamHuman,
  [teamReview.name]: teamReview,
  [qualityScan.name]: qualityScan,
  [dashboardUsage.name]: dashboardUsage,
}
