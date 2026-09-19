import { useEffect, useMemo, useState } from 'react'
import { Alert, Button, Form, Input, Modal, Select, Table, Tag } from 'antd'
import { CheckCircle2, Play, RefreshCw, Send, XCircle, FileWarning, GitMerge } from 'lucide-react'
import { EvidenceBreakdownPanel } from '../components/common/EvidenceBreakdownPanel'
import { MassBalanceWaterfall } from '../components/common/MassBalanceWaterfall'
import { PageHeader } from '../components/common/PageHeader'
import { RecalculationGatePanel } from '../components/common/RecalculationGatePanel'
import { useAuth } from '../hooks/useAuth'
import { useBalanceRun } from '../hooks/useBalanceRun'
import { useTankStore } from '../stores/tankStore'
import type { BalanceRun, BalanceRunInput, BalanceStatus } from '../types/balance'
import { deviationLabels } from '../types/deviation'
import { dateTime, kg, localInputDate, number } from '../utils/format'

const statusLabels: Record<BalanceStatus, string> = {
  queued: '排队',
  calculating: '已计算',
  pending_review: '待复核',
  accepted: '已接受',
  rejected: '已驳回',
  invalidated: '已作废',
  recalculation_required: '待重算',
  replaced: '已替代'
}

const statusColors: Partial<Record<BalanceStatus, string>> = {
  pending_review: 'processing',
  accepted: 'success',
  rejected: 'error',
  invalidated: 'default',
  recalculation_required: 'warning',
  replaced: 'default'
}

export function BalancesPage() {
  const { can } = useAuth()
  const store = useBalanceRun()
  const tanks = useTankStore()
  const [runOpen, setRunOpen] = useState(false)
  const [reviewTarget, setReviewTarget] = useState<'accepted' | 'rejected'>('accepted')
  const [reviewOpen, setReviewOpen] = useState(false)
  const [replaceOpen, setReplaceOpen] = useState(false)
  const [runForm] = Form.useForm<BalanceRunInput>()
  const [reviewForm] = Form.useForm<{ note: string }>()
  useEffect(() => { void Promise.all([store.load(), tanks.load()]) }, [])
  const selected = useMemo(() => store.items.find((item) => item.id === store.selectedId) ?? store.items[0], [store.items, store.selectedId])
  const findRun = (id?: number) => store.items.find((item) => item.id === id)
  const openRun = () => {
    runForm.setFieldsValue({ tank_id: tanks.items[0]?.id })
    setRunOpen(true)
  }
  const run = async (values: BalanceRunInput) => {
    await store.run({
      tank_id: values.tank_id,
      period_start: new Date(values.period_start).toISOString(),
      period_end: new Date(values.period_end).toISOString()
    })
    setRunOpen(false)
  }
  const review = async ({ note }: { note: string }) => {
    if (!selected) return
    await store.review(selected, reviewTarget, note)
    setReviewOpen(false)
    reviewForm.resetFields()
  }
  const recalculate = async (item: BalanceRun) => {
    await store.recalculate(item)
  }
  const replace = async ({ note }: { note: string }) => {
    if (!selected) return
    const successor = findRun(selected.superseded_by_id)
    if (!successor) return
    await store.replace(selected, successor, note)
    setReplaceOpen(false)
    reviewForm.resetFields()
  }
  const openReview = (target: 'accepted' | 'rejected') => {
    setReviewTarget(target)
    setReviewOpen(true)
  }
  const selectedSuccessor = findRun(selected?.superseded_by_id)
  const successorActive = !!selectedSuccessor && !['rejected', 'invalidated', 'replaced'].includes(selectedSuccessor.balance_status)
  const selectedPredecessor = findRun(selected?.supersedes_id)
  return (
    <>
      <PageHeader
        eyebrow="MASS BALANCE WORKBENCH"
        title="平衡工作台"
        description="重放期初、物理转移和期末证据，输出 BOG / 未解释项及其不确定度关系；边界证据变化时由完整性闸门挂起重算。"
        actions={
          <>
            <Button icon={<RefreshCw size={16} />} onClick={() => void store.load()}>刷新</Button>
            {can('process_analyst', 'admin') && <Button type="primary" icon={<Play size={16} />} onClick={openRun}>运行平衡</Button>}
          </>
        }
      />
      <section className="balance-layout">
        <div className="balance-main">
          <div className="chart-panel">
            <div className="section-heading">
              <div>
                <h2>质量边界瀑布</h2>
                <span>{selected ? selected.tank?.tank_code + ' · ' + dateTime(selected.period_end) : '尚未选择运行'}</span>
              </div>
              <div>
                {selected?.supersedes_id && <Tag icon={<GitMerge size={12} />} color="geekblue">重算 #{selected.supersedes_id}</Tag>}
                {selected && <Tag color={statusColors[selected.balance_status] ?? (selected.deviation_level === 'investigate' ? 'warning' : 'success')}>{statusLabels[selected.balance_status]}</Tag>}
                {selected && <Tag color={selected.deviation_level === 'investigate' ? 'warning' : 'success'}>{deviationLabels[selected.deviation_level]}</Tag>}
              </div>
            </div>
            <MassBalanceWaterfall run={selected} />
            {selected && (
              <div className="metric-strip">
                <div><span>BOG / 未解释项</span><strong>{kg(selected.estimated_bog_kg)}</strong></div>
                <div><span>不确定度</span><strong>± {kg(selected.uncertainty_kg)}</strong></div>
                <div><span>偏差率</span><strong>{number.format(selected.deviation_pct)}%</strong></div>
                <div><span>状态</span><strong>{statusLabels[selected.balance_status]}</strong></div>
              </div>
            )}
          </div>
          {selected && (
            <RecalculationGatePanel
              run={selected}
              predecessor={selectedPredecessor}
              successor={selectedSuccessor}
            />
          )}
          {selected?.balance_status === 'recalculation_required' && selectedSuccessor && (
            <Alert
              className="gate-alert"
              type="info"
              showIcon
              message={
                successorActive
                  ? `重算后继 #${selectedSuccessor.id}（${statusLabels[selectedSuccessor.balance_status]}）等待处理`
                  : `重算后继 #${selectedSuccessor.id} 已${statusLabels[selectedSuccessor.balance_status]}，可重新计算`
              }
              description={
                <Button size="small" type="link" onClick={() => store.select(selectedSuccessor.id)}>
                  查看重算后继 #{selectedSuccessor.id}
                </Button>
              }
            />
          )}
          {selected?.balance_status === 'replaced' && selectedSuccessor && (
            <Alert
              className="gate-alert"
              type="info"
              showIcon
              message={`本记录已被重算运行 #${selectedSuccessor.id} 原子替代`}
              description={
                <Button size="small" type="link" onClick={() => store.select(selectedSuccessor.id)}>
                  查看替代后的运行 #{selectedSuccessor.id}（{statusLabels[selectedSuccessor.balance_status]}）
                </Button>
              }
            />
          )}
          {selected && selected.balance_status === 'pending_review' && selectedPredecessor?.balance_status === 'recalculation_required' && (
            <Alert
              className="gate-alert"
              type="info"
              showIcon
              message={`本记录是旧运行 #${selectedPredecessor.id} 的重算后继`}
              description="请在右侧使用「接受并替代旧记录」完成原子闭环；直接接受不会执行替代链更新。"
            />
          )}
          <EvidenceBreakdownPanel run={selected} />
        </div>
        <aside className="run-rail">
          <div className="section-heading"><h2>运行历史</h2><span>{store.items.length} 条</span></div>
          <Table<BalanceRun>
            rowKey="id"
            size="small"
            loading={store.loading}
            dataSource={store.items}
            pagination={{ pageSize: 8, showSizeChanger: false }}
            onRow={(item) => ({ onClick: () => store.select(item.id) })}
            rowClassName={(item) => item.id === selected?.id ? 'selected-row' : ''}
            columns={[
              {
                title: '运行', key: 'run',
                render: (_, item) => (
                  <>
                    <strong>#{item.id} · {item.tank?.tank_code ?? item.tank_id}</strong>
                    {item.supersedes_id && <div className="secondary"><GitMerge size={11} /> 替代 #{item.supersedes_id}</div>}
                    <div className="secondary">{dateTime(item.period_end)}</div>
                  </>
                )
              },
              { title: '状态', dataIndex: 'balance_status', width: 92, render: (value: BalanceStatus) => <Tag color={statusColors[value]}>{statusLabels[value]}</Tag> }
            ]}
          />
          {selected && (
            <div className="workflow-actions">
              {selected.balance_status === 'calculating' && can('process_analyst', 'admin') && <Button type="primary" icon={<Send size={16} />} loading={store.working} onClick={() => void store.submit(selected)} block>提交独立复核</Button>}
              {selected.balance_status === 'recalculation_required' && can('process_analyst', 'admin') && (
                !successorActive
                  ? <Button type="primary" icon={<RefreshCw size={16} />} loading={store.working} onClick={() => void recalculate(selected)} block>
                      {selectedSuccessor ? `重新计算（后继 #${selectedSuccessor.id} 已${statusLabels[selectedSuccessor.balance_status]}）` : '重新计算并生成新记录'}
                    </Button>
                  : <Alert type="warning" showIcon message={<><Button size="small" type="link" style={{ padding: 0 }} onClick={() => store.select(selectedSuccessor!.id)}>重算后继 #{selectedSuccessor!.id}</Button>（{statusLabels[selectedSuccessor.balance_status]}）等待复核员替代</>} />
              )}
              {selected.balance_status === 'pending_review' && can('reviewer', 'admin') && (
                selectedPredecessor?.balance_status === 'recalculation_required'
                  ? <>
                      <Button type="primary" icon={<GitMerge size={16} />} onClick={() => setReplaceOpen(true)} block>接受并替代旧记录 #{selectedPredecessor.id}</Button>
                      <Button danger icon={<XCircle size={16} />} onClick={() => openReview('rejected')} block>驳回重算结果</Button>
                    </>
                  : (
                    <>
                      <Button type="primary" icon={<CheckCircle2 size={16} />} onClick={() => openReview('accepted')} block>接受结果</Button>
                      <Button danger icon={<XCircle size={16} />} onClick={() => openReview('rejected')} block>驳回结果</Button>
                    </>
                  )
              )}
              {selected.balance_status === 'recalculation_required' && can('reviewer', 'admin') && (
                <Alert type="warning" showIcon icon={<FileWarning size={16} />} message="闸门已挂起该记录：复核员不能接受或驳回，等待分析员重新计算。" />
              )}
              {selected.review_note && <Alert type="info" showIcon message={selected.review_note} />}
            </div>
          )}
        </aside>
      </section>
      <Modal title="运行物理质量平衡" open={runOpen} onCancel={() => setRunOpen(false)} footer={null} destroyOnClose>
        <Form<BalanceRunInput>
          form={runForm}
          layout="vertical"
          onFinish={run}
          requiredMark={false}
          initialValues={{
            period_start: localInputDate(new Date(Date.now() - 24 * 3_600_000)),
            period_end: localInputDate(new Date())
          }}
        >
          <Form.Item name="tank_id" label="储罐" rules={[{ required: true }]}><Select options={tanks.items.map((tank) => ({ value: tank.id, label: tank.tank_code + ' · ' + tank.name }))} /></Form.Item>
          <div className="form-grid">
            <Form.Item name="period_start" label="期间开始" rules={[{ required: true }]}><Input type="datetime-local" /></Form.Item>
            <Form.Item name="period_end" label="期间结束" rules={[{ required: true }]}><Input type="datetime-local" /></Form.Item>
          </div>
          <Alert className="form-alert" type="warning" showIcon message="系统将选择期间边界有效快照并固化当前罐容系数；边界证据事后变化会触发完整性闸门，既有结果不会被覆盖。" />
          <Button type="primary" htmlType="submit" icon={<Play size={16} />} loading={store.working} block>执行计算</Button>
        </Form>
      </Modal>
      <Modal title={reviewTarget === 'accepted' ? '接受平衡结果' : '驳回平衡结果'} open={reviewOpen} onCancel={() => setReviewOpen(false)} footer={null} destroyOnClose>
        <Form form={reviewForm} layout="vertical" onFinish={review} requiredMark={false}>
          <Form.Item name="note" label="独立复核意见" rules={[{ required: true, min: 6, max: 1000 }]}><Input.TextArea rows={4} /></Form.Item>
          <Button type={reviewTarget === 'accepted' ? 'primary' : 'default'} danger={reviewTarget === 'rejected'} htmlType="submit" loading={store.working} block>
            确认{reviewTarget === 'accepted' ? '接受' : '驳回'}
          </Button>
        </Form>
      </Modal>
      <Modal
        title={selectedPredecessor ? `接受重算并替代旧记录 #${selectedPredecessor.id}` : '接受重算并替代旧记录'}
        open={replaceOpen}
        onCancel={() => setReplaceOpen(false)}
        footer={null}
        destroyOnClose
      >
        <Alert
          className="form-alert"
          type="info"
          showIcon
          message="接受后原子完成：新记录置为已接受，替代链上全部待重算旧记录置为已替代；重复或并发替代只能成功一次。"
        />
        <Form form={reviewForm} layout="vertical" onFinish={replace} requiredMark={false}>
          <Form.Item name="note" label="独立复核意见" rules={[{ required: true, min: 6, max: 1000 }]}><Input.TextArea rows={4} /></Form.Item>
          <Button type="primary" icon={<GitMerge size={16} />} htmlType="submit" loading={store.working} block>确认接受并替代</Button>
        </Form>
      </Modal>
    </>
  )
}
