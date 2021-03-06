# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

class UserNotifier < ActionMailer::Base
  include AbstractController::Callbacks

  default from: Rails.configuration.Users.UserNotifierEmailFrom

  def account_is_setup(user)
    @user = user
    mail(to: user.email, subject: 'Welcome to Arvados - account enabled')
  end

end
