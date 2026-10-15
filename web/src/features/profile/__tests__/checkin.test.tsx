/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClientProvider, type QueryClient } from '@tanstack/react-query'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { AxiosAdapter } from 'axios'
import i18next from 'i18next'
import { toast } from 'sonner'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import ja from '@/i18n/locales/ja.json'
import ru from '@/i18n/locales/ru.json'
import viLocale from '@/i18n/locales/vi.json'
import zh from '@/i18n/locales/zh.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import { api } from '@/lib/api'
import { createAppQueryClient } from '@/lib/query-client'

import { CheckinCalendarCard } from '../components/checkin-calendar-card'

const eligibilityMessage =
  'Your current balance exceeds the check-in eligibility quota. Please avoid repeated check-ins.'
const translations = {
  en,
  zhCN: zh,
  zhTW,
  fr,
  ja,
  ru,
  vi: viLocale,
}
const originalAdapter = api.defaults.adapter
let client: QueryClient | undefined
let originalLanguage: string
let originalResources: Record<string, Record<string, unknown> | undefined>

beforeEach(async () => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(new Date(2026, 9, 15, 12))
  originalLanguage = i18next.language
  originalResources = {}
  for (const [language, bundle] of Object.entries(translations)) {
    const existing = i18next.getResourceBundle(language, 'translation')
    originalResources[language] = existing
      ? structuredClone(existing)
      : undefined
    i18next.addResourceBundle(language, 'translation', bundle.translation)
  }
  await i18next.changeLanguage('en')
})

afterEach(async () => {
  cleanup()
  client?.clear()
  client = undefined
  api.defaults.adapter = originalAdapter
  for (const [language, bundle] of Object.entries(originalResources)) {
    i18next.removeResourceBundle(language, 'translation')
    if (bundle) i18next.addResourceBundle(language, 'translation', bundle)
  }
  await i18next.changeLanguage(originalLanguage)
  vi.restoreAllMocks()
  vi.useRealTimers()
})

function renderCheckinCard(adapter: AxiosAdapter) {
  api.defaults.adapter = adapter
  client = createAppQueryClient()
  render(
    <QueryClientProvider client={client}>
      <CheckinCalendarCard
        checkinEnabled
        turnstileEnabled={false}
        turnstileSiteKey=''
      />
    </QueryClientProvider>
  )
}

describe('daily check-in eligibility errors', () => {
  it.each(Object.entries(translations))(
    'shows one localized rejection in %s and leaves today available for check-in',
    async (language, bundle) => {
      await i18next.changeLanguage(language)
      const notify = vi.spyOn(toast, 'error').mockReturnValue('checkin-error')
      const success = vi
        .spyOn(toast, 'success')
        .mockReturnValue('checkin-success')
      renderCheckinCard(async (config) => {
        if (
          config.method === 'get' &&
          config.url?.startsWith('/api/user/checkin?')
        ) {
          return {
            data: {
              success: true,
              data: {
                enabled: true,
                stats: {
                  checked_in_today: false,
                  total_checkins: 0,
                  total_quota: 0,
                  checkin_count: 0,
                  records: [],
                },
              },
            },
            status: 200,
            statusText: 'OK',
            headers: {},
            config,
          }
        }
        if (config.method === 'post' && config.url === '/api/user/checkin') {
          return {
            data: {
              success: false,
              message: eligibilityMessage,
              message_key: eligibilityMessage,
            },
            status: 200,
            statusText: 'OK',
            headers: {},
            config,
          }
        }
        throw new Error(`Unexpected request: ${config.method} ${config.url}`)
      })

      const user = userEvent.setup()
      const checkin = await screen.findByRole('button', {
        name: bundle.translation['Check in now'],
      })
      await user.click(checkin)

      await waitFor(() =>
        expect(notify).toHaveBeenCalledExactlyOnceWith(
          bundle.translation[eligibilityMessage]
        )
      )
      expect(checkin).toBeEnabled()
      expect(
        screen.queryByText(bundle.translation['Checked in'])
      ).not.toBeInTheDocument()
      expect(success).not.toHaveBeenCalled()
      if (language === 'zhCN') {
        expect(notify).toHaveBeenCalledWith(
          '当前已有额度已经超过了签到允许额度，请勿频繁签到。'
        )
      }
    }
  )

  it('allows a successful retry after an eligibility rejection and refreshes today as checked in', async () => {
    let attempts = 0
    const notify = vi.spyOn(toast, 'error').mockReturnValue('checkin-error')
    const success = vi
      .spyOn(toast, 'success')
      .mockReturnValue('checkin-success')
    renderCheckinCard(async (config) => {
      if (
        config.method === 'get' &&
        config.url?.startsWith('/api/user/checkin?')
      ) {
        const checkedIn = attempts === 2
        return {
          data: {
            success: true,
            data: {
              enabled: true,
              stats: {
                checked_in_today: checkedIn,
                total_checkins: checkedIn ? 1 : 0,
                total_quota: checkedIn ? 100 : 0,
                checkin_count: checkedIn ? 1 : 0,
                records: checkedIn
                  ? [{ checkin_date: '2026-10-15', quota_awarded: 100 }]
                  : [],
              },
            },
          },
          status: 200,
          statusText: 'OK',
          headers: {},
          config,
        }
      }
      if (config.method === 'post' && config.url === '/api/user/checkin') {
        attempts++
        const data =
          attempts === 1
            ? {
                success: false,
                message: eligibilityMessage,
                message_key: eligibilityMessage,
              }
            : { success: true, data: { quota_awarded: 100 } }
        return { data, status: 200, statusText: 'OK', headers: {}, config }
      }
      throw new Error(`Unexpected request: ${config.method} ${config.url}`)
    })

    const user = userEvent.setup()
    const checkin = await screen.findByRole('button', { name: 'Check in now' })
    await user.click(checkin)
    await waitFor(() =>
      expect(notify).toHaveBeenCalledExactlyOnceWith(eligibilityMessage)
    )
    expect(checkin).toBeEnabled()

    await user.click(checkin)
    expect(
      await screen.findByRole('button', { name: 'Checked in' })
    ).toBeDisabled()
    expect(success).toHaveBeenCalledExactlyOnceWith(
      expect.stringContaining('Check-in successful! Received')
    )
    expect(notify).toHaveBeenCalledTimes(1)
    expect(attempts).toBe(2)
  })
})
