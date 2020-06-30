import React, { useEffect } from 'react';
import { observable, ObservableMap, values } from 'mobx';
import { useStore } from 'store';
import { Channel } from 'store/models';
import { Layout } from 'components/layout';
import LoopPage from 'components/loop/LoopPage';

export default {
  title: 'Pages/Loop',
  component: LoopPage,
  parameters: { contained: true },
};

const channelSubset = (channels: ObservableMap<string, Channel>) => {
  const few = values(channels)
    .slice(0, 20)
    .reduce((result, c) => {
      result[c.chanId] = c;
      return result;
    }, {} as Record<string, Channel>);
  return observable.map(few);
};

export const Default = () => {
  const { channelStore } = useStore();
  useEffect(() => {
    // only use a small set of channels
    channelStore.channels = channelSubset(channelStore.channels);
  }, [channelStore]);

  return <LoopPage />;
};

export const ManyChannels = () => <LoopPage />;

export const InsideLayout = () => {
  const { channelStore } = useStore();
  useEffect(() => {
    // only use a small set of channels
    channelStore.channels = channelSubset(channelStore.channels);
  }, [channelStore]);

  return (
    <Layout>
      <LoopPage />
    </Layout>
  );
};
