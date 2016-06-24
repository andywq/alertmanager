function translateEvent(e) {
  var kinds = {
    "maintain": "维护",
    "abnormal": "异常",
    "prewarning": "预警"
  }

  var levels = {
    "info": "普通",
    "warn": "严重",
    "critical": "重大"
  }

  e.kind = kinds[e.kind]
  e.level = levels[e.level]
  e.is_safe = e.is_safe ? "是" : "否"
  return e
}

angular.module('am.services').factory('Event',
  function($resource) {
    return $resource('', {
      id: '@id'
    }, {
      'query': {
        method: 'GET',
        url: '/api/v1/events'
      },
      'create': {
        method: 'POST',
        url: '/api/v1/events'
      },
      'alerts': {
        method: 'GET',
        url: '/api/v1/event/:id/alerts'
      },
    });
  }
);

angular.module('am.controllers').controller('EventFormCtrl',
  function($scope, $uibModalInstance, Event, initial_alerts) {
    $scope.initial_alerts = initial_alerts
    $scope.selected_alerts = {}
    $scope.e = {}
    var k
    for (k in initial_alerts) {
      $scope.selected_alerts[k] = initial_alerts[k]
    }
    // console.log($scope.selected_alerts);

    $scope.toggleSelectAlert = function(a) {
    if ($scope.selected_alerts[a.id] == undefined) {
      $scope.selected_alerts[a.id] = a;
    } else {
      delete $scope.selected_alerts[a.id];
    }
    // console.log($scope.selected_alerts);
    }

    $scope.create = function() {
      $scope.e.alerts = Object.keys($scope.selected_alerts);
      Event.create($scope.e,
      function(data) {
        // console.log(data);
        $uibModalInstance.close();
      },
      function(data) {
        $scope.error = true;
        // console.log(data);
      }
      );
    }
  }
);

angular.module('am.controllers').controller('EventsCtrl',
  function($scope, Event) {
    $scope.events = [];
    Event.query({},
      function(data) {
        for (i in data.data) {
          var e = translateEvent(data.data[i]);
          e.showAlerts = false;
          $scope.events.push(e);
        }
      },
      function(data) {
        // console.log(data.data); 
    });

    $scope.showAlerts = function(e) {
      if (e.alertObjs == undefined) {
        Event.alerts({id: e.id},
          function(data) {
            // console.log(data);
            e.alertObjs = data.data;
          },
          function(data) {
            // console.log(data.data); 
        });
      }
      e.showAlerts = !e.showAlerts;
    }
  }
);

angular.module('am.directives').directive('alertItem',
  function() {
    return {
      restrict: 'E',
      scope: {
        alert: '='
      },
      templateUrl: 'app/partials/alert-item.html'
    };
  }
);

angular.module('am.controllers').controller('AlertItemCtrl',
  function($scope) {
    $scope.showDetails = false;

    $scope.toggleDetails = function() {
      $scope.showDetails = !$scope.showDetails
    }
  }
);

