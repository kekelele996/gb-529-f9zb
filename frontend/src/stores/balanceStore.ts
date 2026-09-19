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
}

function upsert(items: BalanceRun[], candidate: BalanceRun): BalanceRun[] {
  return [candidate, ...items.filter((item) => item.id !== candidate.id)]
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
    set((state) => ({ items: upsert(state.items, created), selectedId: created.id }))
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
    const result = await api.recalculateBalance(item.id, item.version)
    set((state) => {
      const replaced = state.items
        .map((candidate) => candidate.id === result.superseded.id ? result.superseded : candidate)
      return { items: upsert(replaced, result.recalculated), selectedId: result.recalculated.id }
    })
    return result.recalculated
  }
}))
