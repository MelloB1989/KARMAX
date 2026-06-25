import { Image as RNImage } from 'expo-image';
import React from 'react';
import { StyleSheet } from 'react-native';
import Animated from 'react-native-reanimated';
import { useCssElement } from 'react-native-css';

const AnimatedExpoImage = Animated.createAnimatedComponent(RNImage);

function CSSImage(props: React.ComponentProps<typeof AnimatedExpoImage>) {
  // @ts-expect-error: remap objectFit style to contentFit property
  const { objectFit, objectPosition, ...style } = StyleSheet.flatten(props.style) || {};

  return (
    <AnimatedExpoImage
      contentFit={objectFit}
      contentPosition={objectPosition}
      {...props}
      source={typeof props.source === 'string' ? { uri: props.source } : props.source}
      // @ts-expect-error: style is remapped above
      style={style}
    />
  );
}

export const Image = (
  props: React.ComponentProps<typeof CSSImage> & { className?: string }
) => {
  return useCssElement(CSSImage, props, { className: 'style' });
};

Image.displayName = 'CSS(Image)';
