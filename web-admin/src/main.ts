import { createApp } from 'vue'
import App from './App.vue'
import { router } from './router'
import { restoreSession } from './state/session'
import './styles.css'

async function bootstrap(): Promise<void> {
  // Resolve the persisted refresh token before mounting so the router guard
  // sees the real session state on the first navigation.
  await restoreSession()
  createApp(App).use(router).mount('#app')
}

void bootstrap()
