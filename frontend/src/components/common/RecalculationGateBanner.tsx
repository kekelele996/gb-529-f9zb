import { Alert, Tag } from 'antd'
import { ArrowLeftRight, ShieldAlert } from 'lucide-react'
import type { BalanceRun, RecalculationReason, RecalculationReasonCode } from '../../types/balance'
import { dateTime } from '../../utils/format'

export const recalculationReasonLabels: Record<RecalculationReasonCode, string> = {
  BOUNDARY_SNAPSHOT_CLOSER: '补录更接近边界的有效计量快照',
  LATE_TRANSFER_CONFIRMED: '确认晚录的流入/流出',
  COEFFICIENT_VERSION_CHANGED: '罐容系数版本已更新'
}

interface GateProps {
  run?: BalanceRun
  predecessor?: BalanceRun
  successor?: BalanceRun
}

function ReasonList({ reasons }: { reasons?: RecalculationReason[] }) {
  if (!reasons?.length) return null
  return (
    <ul className="recalc-reason-list">
      {reasons.map((reason) => (
        <li key={reason.code + reason.entity_type + reason.entity_id}>
          <Tag color="orange">{recalculationReasonLabels[reason.code] ?? reason.code}</Tag>
          <span>{reason.detail}</span>
          <span className="secondary">检出时间 {dateTime(reason.detected_at)}</span>
        </li>
      ))}
    </ul>
  )
}

export function RecalculationGateBanner({ run, predecessor, successor }: GateProps) {
  if (!run) return null
  if (run.balance_status === 'recalculate_required') {
    return (
      <Alert
        className="gate-alert"
        type="warning"
        showIcon
        icon={<ShieldAlert size={18} />}
        message="边界证据完整性闸门：待重算，不能接受"
        description={
          <>
            <p>运行存证后出现更接近边界或更新的证据，原运行已冻结为待重算；请由独立复核员执行替代重算，重算将生成新的待复核记录并原子替代本记录。</p>
            <ReasonList reasons={run.recalculation_reason_json} />
          </>
        }
      />
    )
  }
  if (run.balance_status === 'superseded' && successor) {
    return (
      <Alert
        className="gate-alert"
        type="info"
        showIcon
        icon={<ArrowLeftRight size={18} />}
        message={`本记录已被替代重算：后继运行 #${successor.id}`}
        description={
          <>
            <p>
              替代时间 {dateTime(run.recalculated_at ?? successor.recalculated_at ?? run.updated_at)}，后继记录状态为「待复核」，替代前后证据均保留可审计。
            </p>
            <ReasonList reasons={run.recalculation_reason_json} />
          </>
        }
      />
    )
  }
  if (run.supersedes_id && predecessor) {
    return (
      <Alert
        className="gate-alert"
        type="success"
        showIcon
        icon={<ArrowLeftRight size={18} />}
        message={`替代重算记录：源自运行 #${predecessor.id}`}
        description="本记录使用最新边界证据重新计算，原记录已置为已替代，替代链与审计保持完整。"
      />
    )
  }
  return null
}
