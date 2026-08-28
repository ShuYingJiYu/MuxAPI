import { createApp, h } from 'vue'
import { RouterView } from 'vue-router'
import { router } from './router.js'
import { initTheme } from './theme.js'
import './theme.css'
import './style.css'

initTheme()

const app = createApp({ render: () => h(RouterView) })
app.use(router)
router.isReady().then(() => app.mount('#app'))
