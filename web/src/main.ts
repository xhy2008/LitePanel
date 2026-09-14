import { createApp } from 'vue';
import { createPinia } from 'pinia';
import App from './App.vue';
import { makeRouter } from './router';
import './styles/tokens.css';
import './styles/base.css';

createApp(App).use(createPinia()).use(makeRouter()).mount('#app');
