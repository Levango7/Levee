// Logic for the template-instantiation wizard, kept out of the component so it
// can be unit-tested (this repo has no @vue/test-utils).
//
// The wizard exists because the single dialog it replaces let an operator
// submit a change with the template's required parameters blank and with an
// environment typed by hand into a free-text box. The first mistake comes back
// from the server as an error after the round trip; the second is worse — the
// authorization layer judges a change in the environment it DECLARES, so a
// typo silently changes who can act on it, and the change keeps that value for
// the rest of its life.
//
// So the rules here mirror the server's own contract rather than inventing a
// stricter one:
//
//   - a required parameter with an empty value is a BLOCKING issue (the server
//     refuses with ErrRequiredParamMissing, so blocking locally is the same
//     rule, moved earlier);
//   - an empty environment is NOT blocking — a deployment with
//     permission.default_env accepts it — but it is a warning, because the
//     change is then judged by that default for the rest of its life;
//   - team is echoed but not persisted (state.Run has no team column), so the
//     form says so instead of implying the value is recorded.
import type { Template } from '@/types/levee'

export interface InstantiateForm {
  label: string
  params: Record<string, string>
  team: string
  environment: string
  priority: string
  dryRun: boolean
}

export interface FormFinding {
  /** The field the finding is about, for highlighting. */
  field: string
  message: string
}

/** issues blocks submission: the server would refuse the request. */
export function instantiateIssues(form: InstantiateForm, requiredParams: string[]): FormFinding[] {
  const out: FormFinding[] = []
  if (!form.label.trim()) {
    out.push({ field: 'label', message: '变更名称不能为空：看板上没有名字的变更没人能认领' })
  }
  for (const name of requiredParams) {
    if (!(form.params[name] ?? '').trim()) {
      out.push({ field: `param:${name}`, message: `必填参数 ${name} 不能为空` })
    }
  }
  return out
}

/** warnings do not block; they state a consequence the operator should know. */
export function instantiateWarnings(form: InstantiateForm): FormFinding[] {
  const out: FormFinding[] = []
  if (!form.environment.trim()) {
    out.push({
      field: 'environment',
      message:
        '环境留空：该变更将按服务端的默认环境判定授权（未配置默认环境时创建会被拒绝），且这个值此后不可改',
    })
  }
  if (form.team.trim()) {
    out.push({
      field: 'team',
      message: '团队当前会被服务端回显但不落库（state.Run 没有 team 列）；跨队归属按 users.yaml 的主体→团队解析',
    })
  }
  return out
}

/** paramsRows renders the declared parameters in a stable, reviewable order. */
export function paramsRows(
  form: InstantiateForm,
  requiredParams: string[],
): Array<{ name: string; value: string; missing: boolean }> {
  const declared = [...new Set([...requiredParams, ...Object.keys(form.params)])].sort()
  return declared.map((name) => {
    const value = form.params[name] ?? ''
    return { name, value, missing: !value.trim() }
  })
}

/** environmentOptions merges the environments seen in the inventory with the
 *  ones in use, deduped and sorted, for a suggestion list. It is a hint, not a
 *  whitelist: the authorization layer accepts any string, and the inventory is
 *  not exhaustive. */
export function environmentOptions(inventoryEnvs: string[], inUse: string[]): string[] {
  return [...new Set([...inventoryEnvs, ...inUse].map((s) => s.trim()).filter(Boolean))].sort()
}

/** defaultLabel is the label pre-filled when the wizard opens. */
export function defaultLabel(template: Template): string {
  return `${template.name}-${Date.now()}`
}

/** blankForm is the form state the wizard starts from. */
export function blankForm(template: Template): InstantiateForm {
  const params: Record<string, string> = {}
  for (const p of template.requiredParams) params[p] = ''
  return {
    label: defaultLabel(template),
    params,
    team: '',
    environment: '',
    priority: 'normal',
    dryRun: false,
  }
}
