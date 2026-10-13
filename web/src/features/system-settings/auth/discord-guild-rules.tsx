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
import { useFieldArray, useFormContext, useWatch } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import * as z from 'zod'

import { TagInput } from '@/components/tag-input'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'

import type { OAuthFormValues } from './oauth-section'

const discordIdSchema = z.string().refine(
  (value) =>
    /^[1-9][0-9]{0,19}$/.test(value) &&
    BigInt(value) <= 18446744073709551615n,
  'Enter a valid Discord ID'
)

const storedRuleSchema = z
  .object({
    guild_id: discordIdSchema,
    role_ids: z.array(discordIdSchema),
  })
  .strict()

export const discordGuildRulesSchema = z
  .array(
    z
      .object({
        guild_id: discordIdSchema,
        role_ids: z.array(z.string()),
        pending_role: z.string(),
      })
      .superRefine((rule, context) => {
        const roles = [...rule.role_ids]
        if (rule.pending_role.trim()) roles.push(rule.pending_role.trim())
        if (roles.some((role) => !discordIdSchema.safeParse(role).success)) {
          context.addIssue({
            code: 'custom',
            path: ['role_ids'],
            message: 'Enter a valid Discord ID',
          })
        }
      })
  )
  .superRefine((rules, context) => {
    const guilds = new Set<string>()
    rules.forEach((rule, index) => {
      if (guilds.has(rule.guild_id)) {
        context.addIssue({
          code: 'custom',
          path: [index, 'guild_id'],
          message: 'Each Discord server can only appear once',
        })
      }
      guilds.add(rule.guild_id)
    })
  })

export function parseDiscordGuildRules(value: string | undefined): {
  guild_rules: z.infer<typeof discordGuildRulesSchema>
  guild_rules_invalid: boolean
} {
  try {
    const stored = z.array(storedRuleSchema).parse(JSON.parse(value ?? '[]'))
    const guild_rules = stored.map((rule) => ({
      ...rule,
      role_ids: [...new Set(rule.role_ids)],
      pending_role: '',
    }))
    if (!discordGuildRulesSchema.safeParse(guild_rules).success) {
      throw new Error('Invalid rules')
    }
    return { guild_rules, guild_rules_invalid: false }
  } catch {
    // Block submission until the administrator explicitly replaces bad rules.
    return { guild_rules: [], guild_rules_invalid: true }
  }
}

export function serializeDiscordGuildRules(
  rules: z.infer<typeof discordGuildRulesSchema>
): string {
  return JSON.stringify(
    rules.map((rule) => ({
      guild_id: rule.guild_id,
      role_ids: [
        ...new Set(
          [...rule.role_ids, rule.pending_role.trim()].filter(Boolean)
        ),
      ],
    }))
  )
}

export function DiscordGuildRules() {
  const { t } = useTranslation()
  const form = useFormContext<OAuthFormValues>()
  const rules = useFieldArray({
    control: form.control,
    name: 'discord.guild_rules',
  })
  const invalid = useWatch({
    control: form.control,
    name: 'discord.guild_rules_invalid',
  })
  const pendingRoles = useWatch({
    control: form.control,
    name: 'discord.guild_rules',
  })
  const disabled = form.formState.isSubmitting

  return (
    <fieldset className='grid min-w-0 gap-4 lg:col-span-2' disabled={disabled}>
      <legend className='mb-2 text-sm font-medium'>
        {t('Discord server access rules')}
      </legend>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Users must match any server rule and have every listed role in that server. Empty roles require membership only. These rules apply only to sign-in.'
        )}
      </p>
      {invalid ? (
        <Alert variant='destructive'>
          <AlertDescription>
            {t('Stored Discord rules are invalid. Replace them before saving.')}
            <Button
              type='button'
              variant='outline'
              disabled={disabled}
              onClick={() => {
                form.setValue('discord.guild_rules_invalid', false, {
                  shouldDirty: true,
                })
                form.clearErrors('discord.guild_rules_invalid')
                rules.replace([
                  { guild_id: '', role_ids: [], pending_role: '' },
                ])
              }}
            >
              {t('Replace invalid rules')}
            </Button>
          </AlertDescription>
        </Alert>
      ) : (
        <>
          {rules.fields.length === 0 && (
            <p className='text-muted-foreground text-sm'>
              {t('No rules: Discord server access is unrestricted.')}
            </p>
          )}
          {rules.fields.map((rule, index) => (
            <div
              key={rule.id}
              className='grid min-w-0 gap-3 rounded-lg border p-3 sm:grid-cols-2'
            >
              <FormField
                control={form.control}
                name={`discord.guild_rules.${index}.guild_id`}
                render={({ field }) => (
                  <FormItem className='min-w-0'>
                    <FormLabel>{t('Server ID')}</FormLabel>
                    <FormControl>
                      <Input
                        {...field}
                        inputMode='numeric'
                        autoComplete='off'
                        disabled={disabled}
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name={`discord.guild_rules.${index}.role_ids`}
                render={({ field }) => (
                  <FormItem className='min-w-0'>
                    <FormLabel>{t('Required role IDs')}</FormLabel>
                    <FormControl>
                      <TagInput
                        value={field.value}
                        onChange={field.onChange}
                        onBlur={field.onBlur}
                        name={field.name}
                        disabled={disabled}
                        inputMode='numeric'
                        placeholder={t('Add role ID')}
                        inputValue={pendingRoles[index]?.pending_role ?? ''}
                        onInputValueChange={(value) =>
                          form.setValue(
                            `discord.guild_rules.${index}.pending_role`,
                            value,
                            { shouldDirty: true }
                          )
                        }
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <Button
                className='justify-self-start sm:col-span-2'
                type='button'
                variant='outline'
                disabled={disabled}
                onClick={() => rules.remove(index)}
              >
                {t('Remove server rule')}
              </Button>
            </div>
          ))}
          <Button
            className='justify-self-start'
            type='button'
            variant='outline'
            disabled={disabled}
            onClick={() =>
              rules.append({ guild_id: '', role_ids: [], pending_role: '' })
            }
          >
            {t('Add server rule')}
          </Button>
        </>
      )}
    </fieldset>
  )
}
