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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { CheckinSettingsSection } from '../checkin-settings-section'

const clients: QueryClient[] = []

function Fixture(props: { allowedQuota: string; enabled: boolean }) {
  const [container, setContainer] = useState<HTMLDivElement | null>(null)
  return (
    <>
      <div ref={setContainer} />
      <SettingsPageProvider actionsContainer={container}>
        <CheckinSettingsSection
          defaultValues={{
            enabled: props.enabled,
            minQuota: 1000,
            maxQuota: 10000,
            allowedQuota: props.allowedQuota,
          }}
        />
      </SettingsPageProvider>
    </>
  )
}

function renderSettings(allowedQuota = 'null', enabled = true) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  return render(
    <QueryClientProvider client={client}>
      <Fixture allowedQuota={allowedQuota} enabled={enabled} />
    </QueryClientProvider>
  )
}

beforeEach(() => {
  vi.spyOn(api, 'put').mockResolvedValue({ data: { success: true } })
})

afterEach(() => {
  for (const client of clients) client.clear()
  clients.length = 0
})

test.each(['0', '1000', '9007199254740991'])(
  'an unset threshold accepts %s and saves its exact integer string',
  async (value) => {
    const user = userEvent.setup()
    renderSettings()
    const input = screen.getByRole('textbox', {
      name: 'Check-in eligibility quota',
    })
    const save = screen.getByRole('button', { name: 'Save check-in settings' })
    expect(input).toHaveValue('')
    expect(save).toBeDisabled()
    await user.type(input, value)
    expect(save).toBeEnabled()
    await user.click(save)
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: 'Save check-in settings' })
      ).toBeDisabled()
    )
    expect(api.put).toHaveBeenCalledWith('/api/option/', {
      key: 'checkin_setting.allowed_quota',
      value,
    })
    expect(input).toHaveValue(value)
  }
)

test.each(['0', '1000'])(
  'clearing the saved threshold %s persists null rather than zero',
  async (allowedQuota) => {
    const user = userEvent.setup()
    renderSettings(allowedQuota)
    const input = screen.getByRole('textbox', {
      name: 'Check-in eligibility quota',
    })
    expect(input).toHaveValue(allowedQuota)
    await user.clear(input)
    await user.click(
      screen.getByRole('button', { name: 'Save check-in settings' })
    )
    await waitFor(() =>
      expect(api.put).toHaveBeenCalledWith('/api/option/', {
        key: 'checkin_setting.allowed_quota',
        value: 'null',
      })
    )
    expect(input).toHaveValue('')
  }
)

test.each(['-1', '1.5', 'invalid', '1e3', '9007199254740992'])(
  'invalid threshold %s shows a field error and prevents saving',
  async (value) => {
    renderSettings()
    const input = screen.getByRole('textbox', {
      name: 'Check-in eligibility quota',
    })
    fireEvent.change(input, { target: { value } })
    fireEvent.click(
      screen.getByRole('button', { name: 'Save check-in settings' })
    )
    await waitFor(() => expect(input).toHaveAttribute('aria-invalid', 'true'))
    expect(
      screen.getByText(
        'Check-in eligibility quota must be empty or a non-negative safe integer'
      )
    ).toBeInTheDocument()
    expect(input).toHaveValue(value)
    expect(api.put).not.toHaveBeenCalled()
  }
)

test.each(['business', 'network'])(
  'a %s save failure retains the threshold and allows retry',
  async (failure) => {
    if (failure === 'business') {
      vi.mocked(api.put).mockResolvedValueOnce({
        data: { success: false, message: 'Unable to save' },
      })
    } else {
      vi.mocked(api.put).mockRejectedValueOnce(new Error('Unable to save'))
    }
    const user = userEvent.setup()
    renderSettings()
    const input = screen.getByRole('textbox', {
      name: 'Check-in eligibility quota',
    })
    await user.type(input, '1000')
    await user.click(
      screen.getByRole('button', { name: 'Save check-in settings' })
    )
    const retry = await screen.findByRole('button', {
      name: 'Save check-in settings',
    })
    expect(retry).toBeEnabled()
    expect(input).toHaveValue('1000')
    await user.click(retry)
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: 'Save check-in settings' })
      ).toBeDisabled()
    )
    expect(api.put).toHaveBeenCalledTimes(2)
    expect(api.put).toHaveBeenLastCalledWith('/api/option/', {
      key: 'checkin_setting.allowed_quota',
      value: '1000',
    })
  }
)

test('clearing a threshold after saving on the same page persists the new blank value', async () => {
  const user = userEvent.setup()
  renderSettings()
  const input = screen.getByRole('textbox', {
    name: 'Check-in eligibility quota',
  })
  const save = screen.getByRole('button', { name: 'Save check-in settings' })
  await user.type(input, '1000')
  await user.click(save)
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Save check-in settings' })
    ).toBeDisabled()
  )
  await user.clear(input)
  await user.click(save)
  await waitFor(() => expect(api.put).toHaveBeenCalledTimes(2))
  expect(api.put).toHaveBeenLastCalledWith('/api/option/', {
    key: 'checkin_setting.allowed_quota',
    value: 'null',
  })
})

test('enabling check-in reveals the saved zero threshold without clearing it', async () => {
  const user = userEvent.setup()
  renderSettings('0', false)
  expect(
    screen.queryByRole('textbox', { name: 'Check-in eligibility quota' })
  ).not.toBeInTheDocument()
  await user.click(
    screen.getByRole('switch', { name: 'Enable check-in feature' })
  )
  expect(
    screen.getByRole('textbox', { name: 'Check-in eligibility quota' })
  ).toHaveValue('0')
  await user.click(
    screen.getByRole('button', { name: 'Save check-in settings' })
  )
  await waitFor(() => expect(api.put).toHaveBeenCalledTimes(1))
  expect(api.put).toHaveBeenCalledWith('/api/option/', {
    key: 'checkin_setting.enabled',
    value: 'true',
  })
})
