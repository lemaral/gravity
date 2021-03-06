/*
Copyright 2018 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

import reactor from 'app/reactor';
import mainStore from './store';
import settDialogs from './storeDialogs';
import userStore from './users/userStore';
import metricsStore from './metrics/store';
import './logForwarders'
import './tls'

const STORE_NAME = 'settings';

reactor.registerStores({
  [STORE_NAME]: mainStore,
  'settings_dialogs': settDialogs,
  'settings_usrs': userStore,
  'settings_metrics_monitor': metricsStore
});

export const getSettings = () => reactor.evaluate([STORE_NAME]);
export const getClusterName = () => getSettings().getClusterName();