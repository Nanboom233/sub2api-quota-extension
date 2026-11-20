import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUsageCell from '@/components/account/AccountUsageCell.vue'
import UsageProgressBar from '@/components/account/UsageProgressBar.vue'
import { formatDateTimeToMinute } from '@/utils/format'
import type { Account, AccountUsageInfo, UsageProgress } from '@/types'

const { getUsage } = vi.hoisted(() => ({ getUsage: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getUsage } } }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string, values?: { time?: string }) => values?.time ? `${key} ${values.time}` : key }),
}))

const upstreamTitle = 'admin.accounts.usageWindow.upstreamQuota'
const progress = (utilization: number): UsageProgress => ({ utilization, resets_at: null, remaining_seconds: 0 })
const usage = (fields: Partial<AccountUsageInfo>): AccountUsageInfo => ({
  updated_at: null, five_hour: null, seven_day: null, seven_day_sonnet: null, ...fields,
})
const account = (id: number, baseURL = 'https://quota.example/v1'): Account => ({
  id, name: 'quota fixture', platform: 'openai', type: 'apikey', status: 'active',
  credentials: { base_url: baseURL }, concurrency: 1, priority: 1,
  quota_limit: 100, quota_used: 50,
} as Account)

describe('API-key account usage extension', () => {
  beforeEach(() => getUsage.mockReset())

  it('renders native fields, joins batch refresh, and retains local data after silent quota failure', async () => {
    const fixture = account(98001)
    const resetAt = '2099-01-02T03:04:00Z'
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, { props: {
      account: fixture,
      todayStats: { requests: 12, tokens: 300, cost: 1.25, standard_cost: 1.25, user_cost: 1.5 },
      requestBatchedUsage,
      batchedUsage: usage({
        five_hour: { ...progress(10), resets_at: resetAt }, one_day: progress(20), seven_day: progress(30),
        thirty_day: progress(40), quota_total: progress(50), balance: 0,
      }),
    } })
    try {
      await flushPromises()
      expect(requestBatchedUsage).toHaveBeenCalledWith(fixture, undefined)
      const remoteBars = wrapper.findAllComponents(UsageProgressBar).filter(bar => bar.attributes('title') === upstreamTitle)
      expect(remoteBars.map(bar => [bar.props('label'), bar.props('utilization')])).toEqual([
        ['5h', 10], ['1d', 20], ['7d', 30], ['30d', 40], ['total', 50],
      ])
      expect(remoteBars[0]!.text()).toContain(`dashboard.platformQuota.resetsAt ${formatDateTimeToMinute(resetAt)}`)
      expect(remoteBars[1]!.text()).not.toContain('dashboard.platformQuota.resetsAt')
      expect(wrapper.find(`span[title="${upstreamTitle}"]`).text()).toBe('USD 0.00')
      expect(wrapper.text()).toContain('12 req')
      await wrapper.setProps({ manualRefreshToken: 1 })
      expect(requestBatchedUsage).toHaveBeenLastCalledWith(fixture, { force: true })
      await wrapper.setProps({ batchedUsage: usage({}) })
      expect(wrapper.find('.text-amber-600').exists()).toBe(false)
      expect(wrapper.text()).toContain('12 req')
      expect(wrapper.findAllComponents(UsageProgressBar).map(bar => [bar.props('label'), bar.props('utilization')])).toEqual([['total', 50]])
    } finally {
      wrapper.unmount()
    }
  })

  it('loads eligible API-key rows and refreshes after manual or saved key changes', async () => {
    getUsage.mockResolvedValueOnce(usage({ balance: 25 }))
      .mockResolvedValueOnce(usage({ balance: 30 }))
      .mockResolvedValueOnce(usage({ balance: 35 }))
    const fixture = { ...account(98002), updated_at: '2026-10-28T00:00:00Z' }
    const wrapper = mount(AccountUsageCell, { props: { account: fixture } })
    try {
      await flushPromises()
      expect(getUsage).toHaveBeenCalledWith(98002)
      expect(wrapper.text()).toContain('USD 25.00')
      await wrapper.setProps({ manualRefreshToken: 1 })
      await flushPromises()
      expect(getUsage).toHaveBeenCalledTimes(2)
      expect(wrapper.text()).toContain('USD 30.00')
      await wrapper.setProps({ account: { ...fixture, updated_at: '2026-10-28T00:01:00Z' } })
      await flushPromises()
      expect(getUsage).toHaveBeenCalledTimes(3)
      expect(wrapper.text()).toContain('USD 35.00')
    } finally {
      wrapper.unmount()
    }
    const noTarget = mount(AccountUsageCell, { props: { account: account(98003, '') } })
    try {
      await flushPromises()
      expect(getUsage).toHaveBeenCalledTimes(3)
    } finally {
      noTarget.unmount()
    }
  })

  it('keeps multi-protocol rows on their native quota cells', async () => {
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, {
      props: { account: { ...account(98004), platform: 'kimi' }, requestBatchedUsage },
      global: { stubs: { CNProviderQuotaCell: true, CNProviderBalanceCell: true, OllamaCloudUsageCell: true, OpenCodeGoUsageCell: true } },
    })
    try {
      await flushPromises()
      expect(requestBatchedUsage).not.toHaveBeenCalled()
      expect(getUsage).not.toHaveBeenCalled()
    } finally {
      wrapper.unmount()
    }
  })

  it('uses custom quota for a native provider and keeps local data with visible errors', async () => {
    const fixture = { ...account(98006), platform: 'kimi', extra: { quota_script: 'function queryQuota(){}' } } as Account
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, {
      props: { account: fixture, requestBatchedUsage, batchedUsage: usage({ balance: -2, balance_unit: 'credits' }),
        todayStats: { requests: 12, tokens: 300, cost: 1.25, standard_cost: 1.25, user_cost: 1.5 } },
      global: { stubs: { CNProviderQuotaCell: true, CNProviderBalanceCell: true } },
    })
    try {
      await flushPromises()
      expect(requestBatchedUsage).toHaveBeenCalledWith(fixture, undefined)
      expect(wrapper.find('c-n-provider-quota-cell-stub').exists()).toBe(false)
      expect(wrapper.text()).toContain('credits -2.00')
      await wrapper.setProps({ batchedUsage: usage({ error: 'Script failed' }) })
      expect(wrapper.text()).toContain('Script failed')
      expect(wrapper.text()).toContain('12 req')
      expect(wrapper.findAllComponents(UsageProgressBar).map(bar => bar.props('label'))).toEqual(['total'])
    } finally { wrapper.unmount() }
  })

  it('discards an old configuration response and fetches the saved script change', async () => {
    let finishOld!: (value: AccountUsageInfo) => void
    getUsage.mockReturnValueOnce(new Promise(resolve => { finishOld = resolve }))
      .mockResolvedValueOnce(usage({ balance: 7 }))
    const fixture = { ...account(98007), extra: { quota_script: 'function queryQuota(){return {balance:1}}' } }
    const wrapper = mount(AccountUsageCell, { props: { account: fixture } })
    try {
      await flushPromises()
      await wrapper.setProps({ account: { ...fixture, extra: { quota_script: 'function queryQuota(){return {balance:7}}' } } })
      await flushPromises()
      expect(getUsage).toHaveBeenCalledTimes(2)
      expect(wrapper.text()).toContain('USD 7.00')
      finishOld(usage({ balance: 1 }))
      await flushPromises()
      expect(wrapper.text()).not.toContain('USD 1.00')
      expect(wrapper.text()).toContain('USD 7.00')
    } finally { wrapper.unmount() }
  })

  it('retains the native placeholder when a remote response has no displayable data', async () => {
    const wrapper = mount(AccountUsageCell, { props: {
      account: { ...account(98005), quota_limit: undefined, quota_used: undefined },
      requestBatchedUsage: vi.fn(),
      batchedUsage: usage({}),
    } })
    try {
      await flushPromises()
      expect(wrapper.text()).toBe('-')
      await wrapper.setProps({ batchedUsage: usage({ balance: 0 }) })
      expect(wrapper.text()).toBe('USD 0.00')
    } finally {
      wrapper.unmount()
    }
  })

  it.each([
    ['anthropic', 'oauth'], ['anthropic', 'setup-token'], ['gemini', 'oauth'],
  ])('keeps %s %s snapshot refresh passive and ignores unrelated metadata', async (platform, type) => {
    getUsage.mockResolvedValue(usage({ five_hour: progress(21), seven_day: progress(31) }))
    const fixture = { ...account(98100 + (platform === 'gemini' ? 2 : type === 'oauth' ? 0 : 1)),
      platform, type, extra: {}, credentials: {} } as Account
    const wrapper = mount(AccountUsageCell, { props: { account: fixture },
      global: { stubs: { ClaudeResetCreditsCell: true, AccountQuotaInfo: true } } })
    try {
      await flushPromises()
      const changed = { ...fixture, last_used_at: '2026-11-21T01:00:00Z',
        extra: { passive_usage_sampled_at: '2026-11-21T01:00:00Z', session_window_utilization: 0.4 } }
      await wrapper.setProps({ account: changed })
      await flushPromises()
      expect(getUsage).toHaveBeenCalledTimes(2)
      expect(getUsage).toHaveBeenLastCalledWith(...(platform === 'anthropic'
        ? [fixture.id, 'passive', false] : [fixture.id]))
      await wrapper.setProps({ account: { ...changed, updated_at: 'unrelated',
        extra: { ...changed.extra, unrelated: true } } })
      await flushPromises()
      expect(getUsage).toHaveBeenCalledTimes(2)
    } finally { wrapper.unmount() }
  })

  it('keeps unchanged configuration and refreshes after a connection edit', async () => {
    const fixture = { ...account(98103), credentials: { base_url: 'https://quota.example/v1',
      header_overrides: { B: '2', A: '1' } } }
    getUsage.mockResolvedValueOnce(usage({ balance: 11 })).mockResolvedValueOnce(usage({ balance: 12 }))
    const wrapper = mount(AccountUsageCell, { props: { account: fixture } })
    try {
      await flushPromises()
      await wrapper.setProps({ account: { ...fixture,
        credentials: { ...fixture.credentials, header_overrides: { A: '1', B: '2' } } } })
      await flushPromises()
      expect(getUsage).toHaveBeenCalledTimes(1)
      expect(wrapper.text()).toContain('USD 11.00')
      await wrapper.setProps({ account: { ...fixture, proxy_id: 9 } })
      await flushPromises()
      expect(getUsage).toHaveBeenCalledTimes(2)
      expect(wrapper.text()).toContain('USD 12.00')
    } finally { wrapper.unmount() }
  })

  it('rejects an obsolete failure and clears state when a script row stops qualifying', async () => {
    let failOld!: (reason: Error) => void
    getUsage.mockReturnValueOnce(new Promise((_, reject) => { failOld = reject }))
      .mockResolvedValueOnce(usage({ balance: 17 }))
    const fixture = { ...account(98104, ''), extra: { quota_script: 'old script' } }
    const wrapper = mount(AccountUsageCell, { props: { account: fixture } })
    try {
      await flushPromises()
      await wrapper.setProps({ account: { ...fixture, extra: { quota_script: 'saved script' } } })
      await flushPromises()
      failOld(new Error('obsolete failure'))
      await flushPromises()
      expect(wrapper.text()).toContain('USD 17.00')
      expect(wrapper.text()).not.toContain('common.error')
      await wrapper.setProps({ account: { ...fixture, extra: {} } })
      await flushPromises()
      expect(getUsage).toHaveBeenCalledTimes(2)
      expect(wrapper.text()).not.toContain('USD 17.00')
    } finally { wrapper.unmount() }
  })

  it('refreshes saved API-key configuration immediately outside the mobile viewport', async () => {
    const media = vi.spyOn(window, 'matchMedia').mockImplementation(query => ({
      matches: false, media: query, addEventListener: vi.fn(), removeEventListener: vi.fn(),
    }) as unknown as MediaQueryList)
    getUsage.mockResolvedValue(usage({ balance: 23 }))
    const fixture = account(98105)
    const wrapper = mount(AccountUsageCell, { props: { account: fixture } })
    try {
      await flushPromises()
      expect(getUsage).not.toHaveBeenCalled()
      await wrapper.setProps({ account: { ...fixture, updated_at: 'saved' } })
      await flushPromises()
      expect(getUsage).toHaveBeenCalledTimes(1)
      expect(wrapper.text()).toContain('USD 23.00')
    } finally {
      wrapper.unmount()
      media.mockRestore()
    }
  })

  it.each(['success', 'failure'])('rejects an active-query %s after the native snapshot changes', async outcome => {
    let finishOld!: (value: AccountUsageInfo) => void
    let failOld!: (reason: Error) => void
    getUsage.mockResolvedValueOnce(usage({ five_hour: progress(10) }))
      .mockReturnValueOnce(new Promise((resolve, reject) => { finishOld = resolve; failOld = reject }))
      .mockResolvedValueOnce(usage({ five_hour: progress(80) }))
    const fixture = { ...account(98106), platform: 'anthropic', type: 'oauth', credentials: {} } as Account
    const wrapper = mount(AccountUsageCell, { props: { account: fixture }, global: { stubs: {
      ClaudeResetCreditsCell: { template: '<div><slot name="pre-actions" /></div>' },
    } } })
    try {
      await flushPromises()
      await wrapper.get('button').trigger('click')
      expect(getUsage).toHaveBeenLastCalledWith(fixture.id, 'active', true)
      await wrapper.setProps({ account: { ...fixture, last_used_at: 'new passive snapshot' } })
      await flushPromises()
      if (outcome === 'success') finishOld(usage({ five_hour: progress(1) }))
      else failOld(new Error('obsolete active query'))
      await flushPromises()
      expect(wrapper.getComponent(UsageProgressBar).props('utilization')).toBe(80)
      expect(wrapper.text()).not.toContain('common.error')
      expect(wrapper.attributes('aria-busy')).toBe('false')
    } finally { wrapper.unmount() }
  })
})
