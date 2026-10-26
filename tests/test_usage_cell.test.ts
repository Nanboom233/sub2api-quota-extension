import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUsageCell from '@/components/account/AccountUsageCell.vue'
import UsageProgressBar from '@/components/account/UsageProgressBar.vue'
import type { Account, AccountUsageInfo, UsageProgress } from '@/types'

const { getUsage } = vi.hoisted(() => ({ getUsage: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getUsage } } }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key }),
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
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, { props: {
      account: fixture,
      todayStats: { requests: 12, tokens: 300, cost: 1.25, standard_cost: 1.25, user_cost: 1.5 },
      requestBatchedUsage,
      batchedUsage: usage({
        five_hour: progress(10), one_day: progress(20), seven_day: progress(30),
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

  it('loads eligible API-key rows and bypasses the frontend cache on manual refresh', async () => {
    getUsage.mockResolvedValueOnce(usage({ balance: 25 })).mockResolvedValueOnce(usage({ balance: 30 }))
    const wrapper = mount(AccountUsageCell, { props: { account: account(98002) } })
    try {
      await flushPromises()
      expect(getUsage).toHaveBeenCalledWith(98002)
      expect(wrapper.text()).toContain('USD 25.00')
      await wrapper.setProps({ manualRefreshToken: 1 })
      await flushPromises()
      expect(getUsage).toHaveBeenCalledTimes(2)
      expect(wrapper.text()).toContain('USD 30.00')
    } finally {
      wrapper.unmount()
    }
    const noTarget = mount(AccountUsageCell, { props: { account: account(98003, '') } })
    try {
      await flushPromises()
      expect(getUsage).toHaveBeenCalledTimes(2)
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
})
