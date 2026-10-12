import { createApp } from 'vue'
import ElementPlus from 'element-plus'
// Element Plus's Chinese locale. Without it every built-in string stays in the
// library's default English: an empty table reads "No Data", the pager reads
// "Go to", a select with no options reads "No data" — in a console whose own
// copy is all Chinese that is not a style quibble, it is a page that looks
// half-translated to the operator who has to trust it during an incident.
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import 'element-plus/dist/index.css'
// Element Plus's own dark variables. It must load before our stylesheet: our
// tokens.css re-points --el-* under html.dark, and later-in-wins is what makes
// that override stick in dark mode.
import 'element-plus/theme-chalk/dark/css-vars.css'
import * as ElementPlusIconsVue from '@element-plus/icons-vue'

import App from './App.vue'
import router from './router'
import './styles/index.css'

// Application bootstrap. We register Element Plus and its icon components
// globally so that any view can use <el-*> components and icon names without
// per-file imports. The router drives all top-level navigation.
//
// The theme class on <html> is already applied by the inline bootstrap in
// index.html (read the stored choice, resolve, set) — useTheme owns every
// change after that, so nothing here needs to touch it.
const app = createApp(App)

for (const [name, component] of Object.entries(ElementPlusIconsVue)) {
  app.component(name, component)
}

app.use(ElementPlus, { locale: zhCn })
app.use(router)
app.mount('#app')