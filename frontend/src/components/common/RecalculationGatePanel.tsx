import { Alert, Descriptions, Space, Tag, Timeline, Typography } from 'antd'
import { ShieldAlert } from 'lucide-react'
import type { BalanceRun, RecalculationReason, RecalculationReasonCode } from '../../types/balance'
import { dateTime } from '../../utils/format'

const reasonLabels: Record<RecalculationReasonCode, string> = {
  boundary_snapshot_superseded: '边界快照补录',
  late_transfer_confirmed: '晚录转移确认',
  coefficient_version_updated: '罐容系数更新'
}

export const recalculationReasonLabel = (code: RecalculationReasonCode) => reasonLabels[code] ?? code

export function RecalculationGatePanel({
  run,
  predecessor,
  successor
}: {
  run: BalanceRun
  predecessor?: BalanceRun
  successor?: BalanceRun
}) {
  const reasons = run.recalculation_reasons ?? []
  if (run.balance_status !== 'recalculation_required' || reasons.length === 0) return null
  return (
    <Alert
      className="gate-alert"
      type="warning"
      showIcon
      icon={<ShieldAlert size={18} />}
      message="边界证据完整性闸门命中 · 待重算"
      description={
        <Space direction="vertical" size={10} style={{ width: '100%' }}>
          <Timeline
            items={reasons.map((reason: RecalculationReason) => ({
              color: 'orange',
              children: (
                <Space direction="vertical" size={2}>
                  <Space size={8} wrap>
                    <Tag color="orange">{reasonLabels[reason.code] ?? reason.code}</Tag>
                    {reason.evidence_ref && <Typography.Text code>{reason.evidence_ref}</Typography.Text>}
                    <Typography.Text type="secondary">{dateTime(reason.detected_at)}</Typography.Text>
                  </Space>
                  <span>{reason.message}</span>
                </Space>
              )
            }))}
          />
          {successor && (
            <Descriptions size="small" column={1} bordered>
              <Descriptions.Item label="替代链">
                旧运行 #{run.id} → 重算新运行 #{successor.id}（当前状态：{successor.balance_status}）
              </Descriptions.Item>
            </Descriptions>
          )}
          {predecessor && (
            <Descriptions size="small" column={1} bordered>
              <Descriptions.Item label="替代链">
                本重算 #{run.id} 替代待重算旧运行 #{predecessor.id}
              </Descriptions.Item>
            </Descriptions>
          )}
        </Space>
      }
    />
  )
}
