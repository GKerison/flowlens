import { createApp } from 'vue'
import { createPinia } from 'pinia'
import './styles.css'
import App from './App.vue'

// 全局状态按域拆在 stores/{ui,traffic,config}.ts；app.ts 只做装配（init + 事件订阅）。
createApp(App).use(createPinia()).mount('#app')
