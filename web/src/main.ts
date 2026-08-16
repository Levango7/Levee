import { createApp } from 'vue'
import ElementPlus from 'element-plus'
import 'element-plus/dist/index.css'
import * as ElementPlusIconsVue from '@element-plus/icons-vue'

import App from './App.vue'
import router from './router'
import './styles/index.css'

// Application bootstrap. We register Element Plus and its icon components
// globally so that any view can use <el-*> components and icon names without
// per-file imports. The router drives all top-level navigation.
const app = createApp(App)

for (const [name, component] of Object.entries(ElementPlusIconsVue)) {
  app.component(name, component)
}

app.use(ElementPlus)
app.use(router)
app.mount('#app')