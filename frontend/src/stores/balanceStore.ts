import { create } from 'zustand'
import * as api from '../api/balances'
import type { BalanceRun, BalanceRunInput, BalanceStatus } from '../types/balance'

interface BalanceState {
  items: BalanceRun[]
  selectedId: number | null
  loading: boolean
  load: (tankId?: number) => Promise<void>
  select: (id: number) => void
  run: (input: BalanceRunInput) => Promise<BalanceRun>
  submit: (item: BalanceRun) => Promise<BalanceRun>
  review: (item: BalanceRun, target: Extract<BalanceStatus, 'accepted' | 'rejected'>, note: string) => Promise<BalanceRun>
  recalculate: (item: BalanceRun) => Promise<BalanceRun>
  replace: (predecessor: BalanceRun, successor: BalanceRun, note: string) => Promise<BalanceRun>
}

export const useBalanceStore = create<BalanceState>((set) => ({
  items: [],
  selectedId: null,
  loading: false,
  load: async (tankId) => {
    set({ loading: true })
    try {
      const result = await api.listBalances(tankId)
      set((state) => ({ items: result.items, selectedId: state.selectedId ?? result.items[0]?.id ?? null }))
    } finally {
      set({ loading: false })
    }
  },
  select: (id) => set({ selectedId: id }),
  run: async (input) => {
    const created = await api.runBalance(input)
    set((state) => ({ items: [created, ...state.items.filter((item) => item.id !== created.id)], selectedId: created.id }))
    return created
  },
  submit: async (item) => {
    const updated = await api.submitBalance(item.id, item.version)
    set((state) => ({ items: state.items.map((candidate) => candidate.id === updated.id ? updated : candidate), selectedId: updated.id }))
    return updated
  },
  review: async (item, target, note) => {
    const updated = await api.reviewBalance(item.id, item.version, target, note)
    set((state) => ({ items: state.items.map((candidate) => candidate.id === updated.id ? updated : candidate), selectedId: updated.id }))
    return updated
  },
  recalculate: async (item) => {
    const successor = await api.recalculateBalance(item.id, item.version)
    set((state) => {
      const claimed = { ...item, superseded_by_id: successor.id, version: item.version + 1 }
      const items = [successor, ...state.items.map((candidate) => candidate.id === item.id ? claimed : candidate)]
      return { items, selectedId: successor.id }
    })
    return successor
  },
  replace: async (predecessor, successor, note) => {
    const accepted = await api.replaceBalance(predecessor.id, {
      successor_id: successor.id,
      predecessor_version: predecessor.version,
      successor_version: successor.version,
      review_note: note
    })
    set((state) => {
      // 链上全部待重算祖先在后端已一并闭环为 replaced，刷新前先按替代链本地标记。
      const closed = new Set<number>([predecessor.id])
      let anchor: number | undefined = predecessor.supersedes_id
      while (anchor !== undefined) {
        const node = state.items.find((candidate) => candidate.id === anchor)
        if (!node || node.balance_status !== 'recalculation_required') break
        closed.add(node.id)
        anchor = node.supersedes_id
      }
      const items = state.items.map((candidate) => {
        if (candidate.id === successor.id) return accepted
        if (closed.has(candidate.id) && candidate.balance_status === 'recalculation_required') {
          return { ...candidate, balance_status: 'replaced' as BalanceStatus }
        }
        return candidate
      })
      return { items, selectedId: accepted.id }
    })
    return accepted
  }
}))
