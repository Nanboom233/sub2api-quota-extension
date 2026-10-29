import { defineComponent } from 'vue'
import { flushPromises, mount, shallowMount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import QuotaScriptEditor from '@/components/account/QuotaScriptEditor.vue'
import CreateAccountModal from '@/components/account/CreateAccountModal.vue'
import EditAccountModal from '@/components/account/EditAccountModal.vue'
import type { Account, AccountUsageInfo } from '@/types'

const { testQuotaScript, create, update, showError } = vi.hoisted(() => ({
  testQuotaScript: vi.fn(), create: vi.fn(), update: vi.fn(), showError: vi.fn()
}))
vi.mock('@/api/admin', () => ({ adminAPI: {
  accounts: { testQuotaScript, create, update,
    checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }),
    probeUpstreamBilling: vi.fn().mockResolvedValue({}) },
  settings: { getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
    getSettings: vi.fn().mockResolvedValue({}) },
  tlsFingerprintProfiles: { list: vi.fn().mockResolvedValue([]) }
} }))
vi.mock('@/api/admin/accounts', () => ({ getAntigravityDefaultModelMapping: vi.fn().mockResolvedValue([]) }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError, showSuccess: vi.fn(), showWarning: vi.fn(), showInfo: vi.fn() }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isSimpleMode: true }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

const result = (balance: number): AccountUsageInfo => ({
  updated_at: null, five_hour: null, seven_day: null, seven_day_sonnet: null, balance, balance_unit: 'credits'
})
const Dialog = defineComponent({ props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' })
const script = 'function queryQuota(){return {balance:0}}'

describe('quota script editor and forms', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    testQuotaScript.mockReset().mockResolvedValue(result(0))
    create.mockResolvedValue({ id: 1, platform: 'openai', type: 'apikey' })
    update.mockResolvedValue({ id: 1, platform: 'openai', type: 'apikey' })
  })

  it('tests current text, renders results and errors, and ignores stale output', async () => {
    const connection = { api_key: 'draft-key', base_url: 'https://draft.example', proxy_id: null }
    const wrapper = mount(QuotaScriptEditor, { props: { modelValue: '', connection, active: true } })
    try {
      expect(wrapper.get('button').attributes('disabled')).toBeDefined()
      await wrapper.get('textarea').setValue(script)
      expect(wrapper.emitted('update:modelValue')?.[0]).toEqual([script])
      await wrapper.setProps({ modelValue: script })
      await wrapper.get('button').trigger('click')
      await flushPromises()
      expect(testQuotaScript).toHaveBeenLastCalledWith({ ...connection, script })
      expect(wrapper.text()).toContain('credits 0.00')
      testQuotaScript.mockRejectedValueOnce({ message: 'HTTP 403' })
      await wrapper.get('button').trigger('click')
      await flushPromises()
      expect(wrapper.get('[role="alert"]').text()).toBe('HTTP 403')
      expect(showError).not.toHaveBeenCalled()
      let finish!: (value: AccountUsageInfo) => void
      testQuotaScript.mockReturnValueOnce(new Promise(resolve => { finish = resolve }))
      await wrapper.get('button').trigger('click')
      await wrapper.setProps({ connection: { ...connection, base_url: 'https://changed.example' } })
      expect(wrapper.get('button').attributes('disabled')).toBeDefined()
      finish(result(9))
      await flushPromises()
      expect(wrapper.text()).not.toContain('credits 9.00')
      expect(wrapper.find('[role="alert"]').exists()).toBe(false)
      expect(wrapper.get('button').attributes('disabled')).toBeUndefined()
      await wrapper.setProps({ modelValue: '' })
      expect(wrapper.get('button').attributes('disabled')).toBeDefined()
    } finally { wrapper.unmount() }
  })

  it('saves a create draft and supplies its connection to the shared editor', async () => {
    const wrapper = shallowMount(CreateAccountModal, {
      props: { show: true, proxies: [], groups: [] }, global: { stubs: { BaseDialog: Dialog } }
    })
    try {
      const click = async (text: string) => {
        const button = wrapper.findAll('button').find(item => item.text().includes(text))
        expect(button).toBeDefined()
        await button!.trigger('click')
      }
      await click('OpenAI')
      await click('API Key')
      await wrapper.get('form#create-account-form input[type="text"]').setValue('script account')
      await wrapper.get('form#create-account-form input[type="password"]').setValue('draft-key')
      const editor = wrapper.getComponent(QuotaScriptEditor)
      expect(editor.props('connection')).toMatchObject({ api_key: 'draft-key', base_url: 'https://api.openai.com', proxy_id: null })
      editor.vm.$emit('update:modelValue', script)
      await wrapper.get('form#create-account-form').trigger('submit.prevent')
      await flushPromises()
      expect(create).toHaveBeenCalledWith(expect.objectContaining({ type: 'apikey', extra: expect.objectContaining({ quota_script: script }) }))
      expect(testQuotaScript).not.toHaveBeenCalled()
    } finally { wrapper.unmount() }
  })

  it('loads and clears saved code without losing unrelated extra values', async () => {
    const account = {
      id: 1, name: 'script account', platform: 'openai', type: 'apikey', status: 'active',
      credentials: { base_url: 'https://saved.example' }, credentials_status: { has_api_key: true },
      extra: { quota_script: script, unrelated: 'keep' }, proxy_id: null, concurrency: 1, priority: 1,
      rate_multiplier: 1, group_ids: [], expires_at: null, auto_pause_on_expired: false
    } as unknown as Account
    const wrapper = shallowMount(EditAccountModal, {
      props: { show: true, account, proxies: [], groups: [] }, global: { stubs: { BaseDialog: Dialog } }
    })
    try {
      await flushPromises()
      const editor = wrapper.getComponent(QuotaScriptEditor)
      expect(editor.props('modelValue')).toBe(script)
      expect(editor.props('connection')).toMatchObject({ account_id: 1, api_key: '', base_url: 'https://saved.example', proxy_id: null })
      editor.vm.$emit('update:modelValue', '')
      await wrapper.get('form').trigger('submit.prevent')
      await flushPromises()
      expect(update).toHaveBeenCalledWith(1, expect.objectContaining({ extra: expect.objectContaining({ unrelated: 'keep' }) }))
      expect(update.mock.calls[0]?.[1].extra).not.toHaveProperty('quota_script')
      expect(account.extra?.quota_script).toBe(script)
    } finally { wrapper.unmount() }
  })
})
