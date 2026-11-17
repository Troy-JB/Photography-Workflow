import { createRouter, createWebHashHistory } from 'vue-router'
import AlbumView from './views/AlbumView.vue'
import DeliveryView from './views/DeliveryView.vue'
import LinkView from './views/LinkView.vue'
import ProofView from './views/ProofView.vue'

// The share/proof/delivery token lives in the URL fragment, which browsers
// never send to the server; the API only ever sees the exchange that happens in
// JavaScript. The delivery view strips the token after the exchange, so /d
// resumes from the tab's stored session.
export const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', name: 'link', component: LinkView },
    { path: '/a/:token', name: 'album', component: AlbumView, props: true },
    { path: '/p/:token', name: 'proof', component: ProofView, props: true },
    { path: '/d/:token', name: 'delivery', component: DeliveryView, props: true },
    { path: '/d', name: 'delivery-home', component: DeliveryView, props: { token: '' } },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
  scrollBehavior() {
    return { top: 0 }
  },
})
