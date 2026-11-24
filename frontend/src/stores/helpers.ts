/**
 * 各 store 共用的小工具。
 *
 * 单独成文件的原因：traffic / config / app 三处都要把后端错误转成可读文案，
 * 放进任何一个业务 store 都会形成循环引用；ui.ts 只管界面状态，也不该背这个职责。
 */
import { getCurrentLocale, translate } from '../locales'

/** 把后端错误转成可读文案（后端错误文案以中文为主，按当前语言翻译）。 */
export function toMessage(err: unknown): string {
  let text: string
  if (typeof err === 'string') text = err
  else if (err && typeof err === 'object' && 'message' in err) {
    text = String((err as { message: unknown }).message)
  } else text = '操作失败，请查看日志'
  return translate(getCurrentLocale(), text)
}

/** 后端返回的类实例转普通对象，避免响应式代理包裹类导致的比较问题。 */
export function toBrief<T extends object>(raw: T): T {
  return { ...(raw as T) }
}
