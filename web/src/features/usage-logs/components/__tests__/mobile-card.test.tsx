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
import {
  getCoreRowModel,
  useReactTable,
  type VisibilityState,
} from '@tanstack/react-table'
import { render, screen, within, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'

import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { usageLogSchema, type UsageLog } from '../../data/schema'
import { useCommonLogsColumns } from '../columns/common-logs-columns'
import { UsageLogsMobileList } from '../usage-logs-mobile-card'
import {
  UsageLogsProvider,
  useLogsViewScope,
  useUsageLogsContext,
} from '../usage-logs-provider'

const longName = 'enterprise-production-failover-2026-without-any-short-alias'
const log = usageLogSchema.parse({
  id: 1,
  user_id: 2,
  created_at: 1788840000,
  type: 2,
  content: '',
  model_name: longName,
  username: 'production-admin',
  channel: 372,
  channel_name: longName,
  token_name: 'backend-production-token',
  group: 'enterprise-production',
  quota: 123456789,
  use_time: 1.5,
  prompt_tokens: 1200,
  completion_tokens: 800,
  other: JSON.stringify({
    cache_tokens: 300,
    cache_creation_tokens_5m: 200,
    model_ratio: 1,
  }),
})

function Fixture(props: {
  role?: number
  visibility?: VisibilityState
  logs?: UsageLog[]
  loading?: boolean
}) {
  const scope = useLogsViewScope()
  const columns = useCommonLogsColumns(scope.isAdminView, scope.isRootView)
  const context = useUsageLogsContext()
  const table = useReactTable({
    data: props.logs ?? [log],
    columns,
    getCoreRowModel: getCoreRowModel(),
    state: { columnVisibility: props.visibility ?? {} },
  })
  return (
    <>
      <button type='button' onClick={() => context.setSensitiveVisible(false)}>
        Hide sensitive data
      </button>
      <button
        type='button'
        onClick={() =>
          scope.setViewScope(scope.viewScope === 'all' ? 'self' : 'all')
        }
      >
        Switch scope
      </button>
      <UsageLogsMobileList
        table={table}
        logCategory='common'
        isLoading={props.loading}
      />
    </>
  )
}

const clients: QueryClient[] = []

afterEach(() => {
  for (const client of clients) client.clear()
  clients.length = 0
  useAuthStore.setState(useAuthStore.getInitialState(), true)
  localStorage.clear()
})

function renderLogs(props: Parameters<typeof Fixture>[0] = {}) {
  useAuthStore.getState().auth.setUser({
    id: 2,
    username: 'tester',
    role: props.role ?? ROLE.ADMIN,
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  return render(
    <QueryClientProvider client={client}>
      <UsageLogsProvider>
        <Fixture {...props} />
      </UsageLogsProvider>
    </QueryClientProvider>
  )
}

it.each([ROLE.ADMIN, ROLE.SUPER_ADMIN])(
  'shows model mismatch evidence when role %s taps the mobile model badge in all view',
  async (role) => {
    const user = userEvent.setup()
    renderLogs({
      role,
      logs: [
        {
          ...log,
          other: JSON.stringify({
            response_model: {
              requested_model: longName,
              upstream_model: 'mapped-model',
              returned_model: 'unexpected-model',
            },
          }),
        },
      ],
    })
    await user.click(
      screen.getByRole('button', {
        name: `Model: ${longName}, Response model: unexpected-model`,
      })
    )
    const dialog = await screen.findByRole('dialog', { name: 'Model' })
    expect(
      within(dialog).getByText('Response model: unexpected-model')
    ).toBeVisible()
    expect(within(dialog).getByText('mapped-model')).toBeVisible()
    expect(within(dialog).getByText('unexpected-model')).toBeVisible()
  }
)

it('opens long channel text on tap and copies the complete value', async () => {
  const user = userEvent.setup()
  const copy = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
  renderLogs()
  await user.click(
    screen.getByRole('button', { name: `Channel: ${longName} #372` })
  )
  const dialog = await screen.findByRole('dialog', { name: 'Channel' })
  expect(within(dialog).getByText(`${longName} #372`)).toHaveClass(
    '[overflow-wrap:anywhere]'
  )
  await user.click(
    within(dialog).getByRole('button', { name: 'Copy to clipboard' })
  )
  expect(copy).toHaveBeenCalledWith(`${longName} #372`)
  await user.keyboard('{Escape}')
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
  expect(
    screen.getByRole('button', { name: `Channel: ${longName} #372` })
  ).toHaveFocus()
})

it.each([false, true])(
  'copies the full mobile model name with the keyboard without opening details when there is no mapping or difference (response observed: %s)',
  async (observed) => {
    const user = userEvent.setup()
    const copy = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    renderLogs({
      logs: [
        {
          ...log,
          other: observed
            ? JSON.stringify({
                response_model: {
                  requested_model: longName,
                  upstream_model: longName,
                  returned_model: longName,
                },
              })
            : log.other,
        },
      ],
    })
    const button = screen.getByRole('button', { name: `Model: ${longName}` })
    const modelText = within(button).getByText(longName)
    expect(modelText).toHaveClass(
      'line-clamp-2',
      'leading-5',
      '[overflow-wrap:anywhere]'
    )
    expect(modelText.closest('[data-slot="status-badge"]')).toHaveClass(
      'max-w-full',
      'min-h-6',
      'py-px'
    )
    button.focus()
    await user.keyboard('{Enter}')
    expect(copy).toHaveBeenCalledWith(longName)
    expect(
      screen.queryByRole('dialog', { name: 'Model' })
    ).not.toBeInTheDocument()
  }
)

it('hides sensitive names and disables full-text inspection when privacy is enabled', async () => {
  const user = userEvent.setup()
  renderLogs()
  await user.click(screen.getByRole('button', { name: 'Hide sensitive data' }))
  expect(screen.queryByText('production-admin')).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: /Channel:/ })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: /Token:/ })
  ).not.toBeInTheDocument()
})

it('respects hidden columns and omits admin fields in the self view', () => {
  renderLogs({
    role: ROLE.USER,
    visibility: { token_name: false, model_name: false },
  })
  expect(
    screen.queryByRole('button', {
      name: /Channel:|User:|Token:|Group:|Model:/,
    })
  ).not.toBeInTheDocument()
  expect(screen.queryByText('enterprise-production')).not.toBeInTheDocument()
})

it('keeps input, output and cache quantities readable without empty metric cells', () => {
  renderLogs()
  expect(screen.getByText('Input')).toBeVisible()
  expect(screen.getByText('Output')).toBeVisible()
  expect(screen.getByText(/300/)).toBeVisible()
  expect(screen.getByText('Cache ↑ 200')).toBeVisible()
})

it('shows the established empty state when no logs exist', () => {
  renderLogs({ logs: [] })
  expect(screen.getByText('No Logs Found')).toBeVisible()
})

it.each([false, true])(
  'keeps timing in the right column for streaming=%s',
  (streaming) => {
    renderLogs({
      logs: [
        {
          ...log,
          is_stream: streaming,
          use_time: 58,
          other: JSON.stringify({ frt: 58000 }),
        },
      ],
    })
    const row = screen
      .getByRole('button', { name: /^Time:/ })
      .closest('[data-slot="log-time-and-timing"]')
    expect(row).toHaveClass('grid', 'grid-cols-2')
    expect(
      screen.getByRole('button', { name: /^Time:/ }).parentElement
    ).toHaveClass('flex-col', 'justify-between')
    expect(
      within(row as HTMLElement)
        .getByText('Duration')
        .closest('.col-start-2')
    ).not.toBeNull()
    if (streaming) {
      expect(within(row as HTMLElement).getByText('First token')).toBeVisible()
    }
  }
)

it('retains the user avatar and model badge in the mobile summary', () => {
  renderLogs()
  expect(screen.getByText('P')).toBeVisible()
  const modelButton = screen.getByRole('button', { name: `Model: ${longName}` })
  expect(modelButton.querySelector('[data-slot="status-badge"]')).not.toBeNull()
})

it('omits unused token and throughput placeholders for async jobs', () => {
  renderLogs({
    logs: [
      {
        ...log,
        prompt_tokens: 0,
        completion_tokens: 0,
        other: JSON.stringify({ is_task: true }),
      },
    ],
  })
  expect(screen.getByText('Async')).toBeVisible()
  expect(screen.queryByText('Input')).not.toBeInTheDocument()
  const timing = screen
    .getByRole('button', { name: /^Time:/ })
    .closest('[data-slot="log-time-and-timing"]')
  expect(within(timing as HTMLElement).queryByText('—')).not.toBeInTheDocument()
})

it('labels a task whose result was returned in the response as synchronous', () => {
  renderLogs({
    logs: [
      {
        ...log,
        prompt_tokens: 0,
        completion_tokens: 0,
        other: JSON.stringify({ is_task: true, task_sync: true }),
      },
    ],
  })
  expect(screen.getByText('Sync')).toBeVisible()
  expect(screen.queryByText('Async')).not.toBeInTheDocument()
})

it('shows mapped model names in full when inspecting a mobile model badge', async () => {
  const user = userEvent.setup()
  renderLogs({
    logs: [
      {
        ...log,
        other: JSON.stringify({
          is_model_mapped: true,
          upstream_model_name:
            'provider-production-mapped-model-with-a-long-name',
        }),
      },
    ],
  })
  await user.click(screen.getByRole('button', { name: `Model: ${longName}` }))
  const dialog = await screen.findByRole('dialog', { name: 'Model' })
  expect(within(dialog).getByText('Actual Model')).toBeVisible()
  expect(
    within(dialog).getByText(
      'provider-production-mapped-model-with-a-long-name'
    )
  ).toBeVisible()
})

it('shows loading placeholders without displaying stale log fields', () => {
  renderLogs({ loading: true })
  expect(screen.getByRole('status', { name: 'Loading' })).toHaveAttribute(
    'aria-busy',
    'true'
  )
  expect(
    screen.queryByRole('button', { name: /^Model:/ })
  ).not.toBeInTheDocument()
})

const modelMetadata = {
  is_model_mapped: true,
  upstream_model_name: 'mapped-model',
  response_model: {
    requested_model: 'observation-request-model',
    upstream_model: 'mapped-model',
    returned_model: 'unexpected-model',
  },
}

it.each([
  { role: ROLE.USER, self: false, other: modelMetadata },
  {
    role: ROLE.USER,
    self: false,
    other: { is_model_mapped: true, upstream_model_name: 'mapped-model' },
  },
  { role: ROLE.ADMIN, self: true, other: modelMetadata },
  { role: ROLE.SUPER_ADMIN, self: true, other: modelMetadata },
])(
  'only shows and copies the request model in restricted mobile view (role=$role, self=$self, metadata=$other)',
  async ({ role, self, other }) => {
    const user = userEvent.setup()
    const copy = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    renderLogs({ role, logs: [{ ...log, other: JSON.stringify(other) }] })
    if (self) {
      await user.click(screen.getByRole('button', { name: 'Switch scope' }))
    }

    const model = screen.getByRole('button', { name: `Model: ${longName}` })
    expect(model).not.toHaveAttribute('aria-haspopup')
    expect(screen.queryByText('mapped-model')).not.toBeInTheDocument()
    expect(screen.queryByText(/unexpected-model/)).not.toBeInTheDocument()
    expect(
      screen.queryByText(/observation-request-model/)
    ).not.toBeInTheDocument()
    await user.click(model)
    expect(copy).toHaveBeenLastCalledWith(longName)
    expect(
      screen.queryByRole('dialog', { name: 'Model' })
    ).not.toBeInTheDocument()
    model.focus()
    await user.keyboard('{Enter}')
    expect(copy).toHaveBeenLastCalledWith(longName)
  }
)

it.each([ROLE.ADMIN, ROLE.SUPER_ADMIN])(
  'removes model diagnostics when role %s switches to self and restores them in all view',
  async (role) => {
    const user = userEvent.setup()
    renderLogs({ role, logs: [{ ...log, other: JSON.stringify(modelMetadata) }] })
    const warning = 'Response model: unexpected-model'
    expect(screen.getByText(warning)).toBeVisible()
    await user.click(screen.getByRole('button', { name: 'Switch scope' }))
    expect(screen.queryByText(warning)).not.toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: `Model: ${longName}` })
    ).not.toHaveAttribute('aria-haspopup')
    await user.click(screen.getByRole('button', { name: 'Switch scope' }))
    expect(screen.getByText(warning)).toBeVisible()
    await user.click(
      screen.getByRole('button', {
        name: `Model: ${longName}, ${warning}`,
      })
    )
    const dialog = await screen.findByRole('dialog', { name: 'Model' })
    expect(within(dialog).getByText('mapped-model')).toBeVisible()
    expect(within(dialog).getByText('unexpected-model')).toBeVisible()
  }
)
