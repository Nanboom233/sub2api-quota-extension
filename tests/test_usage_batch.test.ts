import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import AccountsView from '@/views/admin/AccountsView.vue'
import AccountUsageCell from '@/components/account/AccountUsageCell.vue'
import type { Account, AccountUsageInfo } from '@/types'

const { list, getBatchUsage, getBatchTodayStats, getSettings, getProxies, getGroups } = vi.hoisted(() => ({
  list: vi.fn(), getBatchUsage: vi.fn(), getBatchTodayStats: vi.fn(),
  getSettings: vi.fn(), getProxies: vi.fn(), getGroups: vi.fn(),
}))
vi.mock('@/api/admin', () => ({ adminAPI: {
  accounts: { list, getBatchUsage, listWithEtag: vi.fn(),
    getBatchTodayStats, getUpstreamBillingProbeSettings: getSettings },
  proxies: { getAll: getProxies }, groups: { getAll: getGroups },
} }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({
  showError: vi.fn(), showWarning: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn(),
}) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ token: 'synthetic', isSimpleMode: false }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key }),
}))

const DataTable = defineComponent({
  props: { data: { type: Array, default: () => [] }, loading: Boolean },
  template: '<div><template v-if="!loading"><div v-for="row in data" :key="row.id"><slot name="cell-usage" :row="row" /></div></template></div>',
})
const row = (id: number): Account => ({
  id, name: `quota ${id}`, platform: 'openai', type: 'apikey', status: 'active',
  schedulable: true, concurrency: 1, priority: 1, extra: { quota_script: 'original script' },
  credentials: { base_url: 'https://quota.example/v1', api_key: 'redacted' },
} as Account)
const usage = (balance: number): AccountUsageInfo => ({
  updated_at: null, five_hour: null, seven_day: null, seven_day_sonnet: null, balance,
})
type BatchResult = { usage: Record<string, AccountUsageInfo>; errors: Record<string, string> }
const result = (balance: number): BatchResult => ({ usage: { '42': usage(balance) }, errors: {} })
function deferred() {
  let resolve!: (value: BatchResult) => void
  let reject!: (reason: Error) => void
  const promise = new Promise<BatchResult>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
function mountView(rows = [row(42)]) {
  list.mockResolvedValue({ items: rows, total: rows.length, page: 1, page_size: 20, pages: 1 })
  return mount(AccountsView, { global: { stubs: {
    AppLayout: { template: '<div><slot /></div>' },
    TablePageLayout: { template: '<div><slot name="table" /></div>' }, DataTable,
    AccountBulkActionsBar: true, Pagination: true, ConfirmDialog: true,
    ImportDataModal: true, ReAuthAccountModal: true, AccountTestModal: true,
    AccountStatsModal: true, ScheduledTestsPanel: true, SyncFromCrsModal: true,
    TempUnschedStatusModal: true, ErrorPassthroughRulesModal: true,
    TLSFingerprintProfilesModal: true, CreateAccountModal: true,
    EditAccountModal: true, BulkEditAccountModal: true,
  } } })
}
async function flushBatch() {
  await flushPromises()
  await vi.advanceTimersByTimeAsync(1)
  await flushPromises()
}

describe('unified account batch usage refresh', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    localStorage.clear()
    list.mockReset()
    getBatchUsage.mockReset()
    getBatchTodayStats.mockResolvedValue({ stats: {} })
    getSettings.mockResolvedValue({ enabled: false })
    getProxies.mockResolvedValue([])
    getGroups.mockResolvedValue([])
  })
  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it.each(['success', 'failure'])('rejects an old-key %s before the cell watcher advances its token', async outcome => {
    const oldRequest = deferred()
    const newRequest = deferred()
    getBatchUsage.mockReturnValueOnce(oldRequest.promise).mockReturnValueOnce(newRequest.promise)
    vi.spyOn(console, 'error').mockImplementation(() => {})
    const wrapper = mountView()
    try {
      await flushBatch()
      const cell = wrapper.getComponent(AccountUsageCell)
      // Queue the reply before Vue's watcher flush. The parent already sees the edited key.
      if (outcome === 'success') oldRequest.resolve(result(1))
      else oldRequest.reject(new Error('obsolete request'))
      cell.props('account').extra = { quota_script: 'saved script' }
      await flushBatch()
      expect(getBatchUsage).toHaveBeenCalledTimes(2)
      expect(getBatchUsage).toHaveBeenLastCalledWith([42], true)
      expect(cell.props('batchedUsage')).toBeNull()
      expect(cell.props('batchedUsageError')).toBeNull()
      expect(cell.props('batchedUsageLoading')).toBe(true)
      newRequest.resolve(result(7))
      await flushPromises()
      expect(cell.props('batchedUsage')?.balance).toBe(7)
      expect(cell.props('batchedUsageError')).toBeNull()
      expect(cell.props('batchedUsageLoading')).toBe(false)
    } finally { wrapper.unmount() }
  })

  it('refreshes one edited cached account and reuses the unaffected account cache', async () => {
    getBatchUsage.mockResolvedValueOnce({ usage: { '42': usage(11), '43': usage(22) }, errors: {} })
      .mockResolvedValueOnce(result(33))
    const wrapper = mountView([row(42), row(43)])
    try {
      await flushBatch()
      expect(getBatchUsage).toHaveBeenCalledWith([42, 43], false)
      const [changed, unchanged] = wrapper.findAllComponents(AccountUsageCell)
      changed!.props('account').updated_at = 'saved hidden key'
      await flushBatch()
      expect(getBatchUsage).toHaveBeenCalledTimes(2)
      expect(getBatchUsage).toHaveBeenLastCalledWith([42], true)
      expect(changed!.props('batchedUsage')?.balance).toBe(33)
      expect(unchanged!.props('batchedUsage')?.balance).toBe(22)
      unchanged!.props('requestBatchedUsage')!(unchanged!.props('account'))
      await flushBatch()
      expect(getBatchUsage).toHaveBeenCalledTimes(2)
    } finally { wrapper.unmount() }
  })

  it('keeps native request tokens for overlapping manual refreshes with the same key', async () => {
    const oldRequest = deferred()
    const newRequest = deferred()
    getBatchUsage.mockReturnValueOnce(oldRequest.promise).mockReturnValueOnce(newRequest.promise)
    const wrapper = mountView()
    try {
      await flushBatch()
      const cell = wrapper.getComponent(AccountUsageCell)
      cell.props('requestBatchedUsage')!(cell.props('account'), { force: true })
      await flushBatch()
      newRequest.resolve(result(9))
      await flushPromises()
      oldRequest.resolve(result(1))
      await flushPromises()
      expect(getBatchUsage).toHaveBeenCalledTimes(2)
      expect(cell.props('batchedUsage')?.balance).toBe(9)
      expect(cell.props('batchedUsageError')).toBeNull()
      expect(cell.props('batchedUsageLoading')).toBe(false)
    } finally { wrapper.unmount() }
  })
})
