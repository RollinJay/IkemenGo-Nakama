//
//  TouchOverlayViewController.m
//  I.K.E.M.E.N-Go
//
//  Created by Jesuszilla on 8/25/26.
//

#import "TouchOverlayViewController.h"
#import "TouchOverlayView.h"
#import "ViewController.h"
#import <UIKit/UIKit.h>
#import <Foundation/Foundation.h>

@interface TouchOverlayViewController ()
@end

@implementation TouchOverlayViewController

- (void)viewDidLoad {
    [super viewDidLoad];
}

// modiified from https://stackoverflow.com/a/49503308
- (void)awakeFromNib {
    [super awakeFromNib];
    NSBundle *bundle = [NSBundle bundleForClass:[self class]];
    UINib *nib = [UINib nibWithNibName:@"TouchOverlayView" bundle:bundle];
    
    UIView *placeholder = [[nib instantiateWithOwner:self.view options:nil] firstObject];
    placeholder.translatesAutoresizingMaskIntoConstraints = NO;
    [self.view insertSubview:placeholder aboveSubview:self.view];
    [NSLayoutConstraint activateConstraints:@[
        [placeholder.topAnchor constraintEqualToAnchor:self.view.topAnchor],
        [placeholder.bottomAnchor constraintEqualToAnchor:self.view.bottomAnchor],
        [placeholder.leadingAnchor constraintEqualToAnchor:self.view.leadingAnchor],
        [placeholder.trailingAnchor constraintEqualToAnchor:self.view.trailingAnchor]
    ]];
    self.overlayView = (TouchOverlayView*)self.view;
}

// THIS! YOU! YOU FUCKING GUY! THIS IS THE KEY! YOU DO NOT EVER TELL ME TO FUCK!
- (void)setParentVC:(ViewController *)parentVC {
    _parentVC = parentVC;
    self.overlayView.delegate = _parentVC;
}

@end
